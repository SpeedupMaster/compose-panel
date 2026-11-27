import test from 'node:test'
import assert from 'node:assert/strict'
import {
  advanceLogCursor,
  archiveReasonLabel,
  archiveTargetKey,
  archiveTargets,
  extractLogTimestamp,
  filterArchivesByTarget,
  formatArchiveOption,
  formatLogCursor,
  nextArchiveAfterDelete,
  resolveActiveArchive,
  resolveArchiveTarget,
  shouldResumeLiveLogs,
  stripDockerTimestamp,
} from '../src/components/logStream.ts'

test('extractLogTimestamp and advanceLogCursor track nanosecond timestamps and same-ns sequences', () => {
  assert.equal(extractLogTimestamp('2026-11-27T02:51:13.123456789Z hello world'), '2026-11-27T02:51:13.123456789Z')
  assert.equal(extractLogTimestamp('plain line without ts'), '')

  let cur = null
  cur = advanceLogCursor(cur, 'plain before')
  assert.equal(cur, null)

  cur = advanceLogCursor(cur, '2026-11-27T02:51:13.100000000Z line 1')
  assert.equal(formatLogCursor(cur), '2026-11-27T02:51:13.100000000Z|1')

  // Same nanosecond increments seq.
  cur = advanceLogCursor(cur, '2026-11-27T02:51:13.100000000Z line 2')
  assert.equal(formatLogCursor(cur), '2026-11-27T02:51:13.100000000Z|2')

  // Trailing-zero equivalent nanosecond also counts as same nanosecond.
  cur = advanceLogCursor(cur, '2026-11-27T02:51:13.1Z line 3')
  assert.equal(formatLogCursor(cur), '2026-11-27T02:51:13.100000000Z|3')

  // Out-of-order older timestamp must NOT rewind cursor.
  cur = advanceLogCursor(cur, '2026-11-27T02:51:12.900000000Z older stderr')
  assert.equal(formatLogCursor(cur), '2026-11-27T02:51:13.100000000Z|3')

  // Newer timestamp resets seq to 1.
  cur = advanceLogCursor(cur, '2026-11-27T02:51:13.100000001Z next ns')
  assert.equal(formatLogCursor(cur), '2026-11-27T02:51:13.100000001Z|1')
})

test('shouldResumeLiveLogs handles false->true start, fast restart startedAt change, and restartCount bump', () => {
  const runningT0 = { running: true, startedAt: '2026-11-27T01:00:00Z', restartCount: 0 }
  const stoppedT0 = { running: false, startedAt: '2026-11-27T01:00:00Z', restartCount: 0 }
  const runningT1 = { running: true, startedAt: '2026-11-27T01:05:00Z', restartCount: 0 }
  const crashRestarted = { running: true, startedAt: '2026-11-27T01:00:00Z', restartCount: 1 }

  // Not ended or follow disabled -> never resume.
  assert.equal(shouldResumeLiveLogs(runningT0, runningT1, false, true), false)
  assert.equal(shouldResumeLiveLogs(runningT0, runningT1, true, false), false)

  // Stopped container -> never resume.
  assert.equal(shouldResumeLiveLogs(runningT0, stoppedT0, true, true), false)

  // Same snapshot after end -> do not tight-loop reconnect.
  assert.equal(shouldResumeLiveLogs(runningT0, runningT0, true, true), false)

  // Stopped -> started transition -> resume!
  assert.equal(shouldResumeLiveLogs(stoppedT0, runningT0, true, true), true)

  // Fast in-place restart (running stayed true, startedAt changed) -> resume!
  assert.equal(shouldResumeLiveLogs(runningT0, runningT1, true, true), true)

  // Crash restart bumping restartCount -> resume!
  assert.equal(shouldResumeLiveLogs(runningT0, crashRestarted, true, true), true)
})

test('archiveReasonLabel, formatArchiveOption, and stripDockerTimestamp', () => {
  assert.equal(archiveReasonLabel('deploy'), '部署前')
  assert.equal(archiveReasonLabel('auto_update'), '自动更新前')
  assert.equal(archiveReasonLabel('rollback'), '回滚前')
  assert.equal(archiveReasonLabel('recreate'), '重建前')
  assert.equal(archiveReasonLabel('down'), '下线前')
  assert.equal(archiveReasonLabel('remove'), '删除前')

  const label = formatArchiveOption(
    {
      id: 'cla_1',
      project: 'demo',
      service: 'web',
      containerId: 'abcdef123456',
      containerName: 'demo-web-1',
      image: 'nginx:latest',
      reason: 'auto_update',
      startedAt: 1795660000,
      archivedAt: 1795661473,
      sizeBytes: 2048,
      rawBytes: 12000,
      lineCount: 142,
      truncated: false,
    },
    true,
  )
  assert.match(label, /web/)
  assert.match(label, /自动更新前/)
  assert.match(label, /142 行/)

  assert.equal(stripDockerTimestamp('2026-11-27T02:51:13.123456789Z hello'), 'hello')
  assert.equal(stripDockerTimestamp('no-timestamp line'), 'no-timestamp line')
})

test('archiveTargetKey, archiveTargets, filterArchivesByTarget, and nextArchiveAfterDelete', () => {
  const mk = (id, project, service, containerName) => ({
    id,
    project,
    service,
    containerId: 'cid-' + id,
    containerName,
    image: 'img:1',
    reason: 'deploy',
    startedAt: 100,
    archivedAt: 200,
    sizeBytes: 100,
    rawBytes: 300,
    lineCount: 10,
    truncated: false,
  })
  const list = [
    mk('cla_1', 'proj-a', 'web', 'proj-a-web-1'),
    mk('cla_2', 'proj-b', 'web', 'proj-b-web-1'),
    mk('cla_3', '', '', 'standalone-1'),
  ]

  // Project-scoped target key omits project name; global view includes project prefix to distinguish same service names.
  assert.equal(archiveTargetKey(list[0], false), 'web')
  assert.equal(archiveTargetKey(list[0], true), 'proj-a/web')
  assert.equal(archiveTargetKey(list[2], true), 'standalone-1')

  assert.deepEqual(archiveTargets(list, true), ['proj-a/web', 'proj-b/web', 'standalone-1'])
  assert.deepEqual(filterArchivesByTarget(list, 'proj-b/web', true).map((a) => a.id), ['cla_2'])
  assert.deepEqual(filterArchivesByTarget(list, '', true).map((a) => a.id), ['cla_1', 'cla_2', 'cla_3'])

  assert.equal(nextArchiveAfterDelete(list, 'cla_1', 'cla_1'), 'cla_2')
  assert.equal(nextArchiveAfterDelete(list, 'cla_2', 'cla_1'), 'cla_1')
  assert.equal(nextArchiveAfterDelete([list[0]], 'cla_1', 'cla_1'), '')

  // Deleting the last archive of a filtered target must reset effective target back to '' (all)
  // so the modal cannot get deadlocked on an empty filtered list when targets.length drops to 1.
  assert.equal(resolveArchiveTarget(['proj-a/web', 'proj-b/web'], 'proj-b/web'), 'proj-b/web')
  assert.equal(resolveArchiveTarget(['proj-a/web'], 'proj-b/web'), '')
  assert.equal(resolveArchiveTarget([], 'proj-b/web'), '')

  // Switching to a different archive must never display stale fetched metadata from the previous archive.
  assert.equal(resolveActiveArchive(list[0], list, 'cla_2')?.id, 'cla_2')
  assert.equal(resolveActiveArchive(list[0], list, 'cla_1')?.id, 'cla_1')
  assert.equal(resolveActiveArchive(list[0], [], 'cla_2'), null)
})
