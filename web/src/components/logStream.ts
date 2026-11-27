// Pure helpers for container log SSE cursor tracking, restart resume detection, and log archives.

export interface LogArchiveItem {
  id: string
  project: string
  service: string
  containerId: string
  containerName: string
  image: string
  reason: string
  startedAt: number
  archivedAt: number
  sizeBytes: number
  rawBytes: number
  lineCount: number
  truncated: boolean
}

export interface LogCursor {
  ts: string
  seq: number
}

export interface LiveStreamSnapshot {
  running: boolean
  startedAt: string
  restartCount: number
}

const DOCKER_TS_RE = /^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})(?:\.(\d+))?(Z|[+-]\d{2}:\d{2})(?:\s|$)/

export function extractLogTimestamp(line: string): string {
  const m = DOCKER_TS_RE.exec(line)
  if (!m) return ''
  return `${m[1]}${m[2] ? '.' + m[2] : ''}${m[3]}`
}

function parseRFC3339NanoParts(ts: string): { sec: number; nano: number } | null {
  const m = DOCKER_TS_RE.exec(ts)
  if (!m) return null
  const ms = Date.parse(`${m[1]}${m[3]}`)
  if (Number.isNaN(ms)) return null
  const sec = Math.floor(ms / 1000)
  const nano = m[2] ? parseInt(m[2].padEnd(9, '0').slice(0, 9), 10) : 0
  return { sec, nano }
}

function compareRFC3339Nano(a: string, b: string): number {
  const pa = parseRFC3339NanoParts(a)
  const pb = parseRFC3339NanoParts(b)
  if (!pa || !pb) return a < b ? -1 : a > b ? 1 : 0
  if (pa.sec !== pb.sec) return pa.sec < pb.sec ? -1 : 1
  if (pa.nano !== pb.nano) return pa.nano < pb.nano ? -1 : 1
  return 0
}

export function advanceLogCursor(prev: LogCursor | null, line: string): LogCursor | null {
  const ts = extractLogTimestamp(line)
  if (!ts) return prev
  if (!prev) return { ts, seq: 1 }
  const cmp = compareRFC3339Nano(ts, prev.ts)
  if (cmp > 0) return { ts, seq: 1 }
  if (cmp === 0) return { ts: prev.ts, seq: prev.seq + 1 }
  return prev
}

export function formatLogCursor(cursor: LogCursor | null): string {
  return cursor ? `${cursor.ts}|${cursor.seq}` : ''
}

export function shouldResumeLiveLogs(
  atStreamStart: LiveStreamSnapshot | null,
  current: LiveStreamSnapshot,
  ended: boolean,
  follow: boolean,
): boolean {
  if (!ended || !follow || !current.running || !atStreamStart) return false
  if (!atStreamStart.running && current.running) return true
  if (current.startedAt !== '' && current.startedAt !== atStreamStart.startedAt) return true
  if (current.restartCount > atStreamStart.restartCount) return true
  return false
}

const REASON_LABELS: Record<string, string> = {
  deploy: '部署前',
  auto_update: '自动更新前',
  rollback: '回滚前',
  recreate: '重建前',
  down: '下线前',
  remove: '删除前',
}

export function archiveReasonLabel(reason: string): string {
  return REASON_LABELS[reason] || reason || '历史归档'
}

export function formatArchiveShortTime(tsSec: number): string {
  if (!tsSec) return '—'
  const d = new Date(tsSec * 1000)
  const p = (n: number) => String(n).padStart(2, '0')
  return `${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`
}

export function archiveTargetKey(a: LogArchiveItem, showProject = false): string {
  const leaf = a.service || a.containerName
  if (showProject && a.project) {
    return `${a.project}/${leaf}`
  }
  return leaf
}

export function archiveTargets(archives: LogArchiveItem[], showProject = false): string[] {
  return [...new Set(archives.map((a) => archiveTargetKey(a, showProject)).filter(Boolean))].sort((a, b) => a.localeCompare(b))
}

export function formatArchiveOption(a: LogArchiveItem, includeTarget = false, showProject = false): string {
  const target = archiveTargetKey(a, showProject)
  const prefix = includeTarget && target ? `${target} · ` : ''
  return `${prefix}${archiveReasonLabel(a.reason)} · ${formatArchiveShortTime(a.archivedAt)}（${a.lineCount} 行）`
}

export function stripDockerTimestamp(line: string): string {
  return line.replace(/^\d{4}-\d\d-\d\dT[\d:.]+(?:Z|[+-]\d\d:\d\d)\s?/, '')
}

export function filterArchivesByTarget(archives: LogArchiveItem[], target: string, showProject = false): LogArchiveItem[] {
  if (!target) return archives
  return archives.filter((a) => archiveTargetKey(a, showProject) === target)
}

export function nextArchiveAfterDelete(archives: LogArchiveItem[], deletedId: string, currentId: string): string {
  const remaining = archives.filter((a) => a.id !== deletedId)
  if (currentId !== deletedId) return currentId
  return remaining[0]?.id || ''
}

export function resolveArchiveTarget(targets: string[], currentTarget: string): string {
  if (!currentTarget) return ''
  return targets.includes(currentTarget) ? currentTarget : ''
}

export function resolveActiveArchive(
  fetched: LogArchiveItem | null,
  list: LogArchiveItem[],
  selectedId: string,
): LogArchiveItem | null {
  if (!selectedId) return null
  if (fetched && fetched.id === selectedId) return fetched
  return list.find((a) => a.id === selectedId) || null
}
