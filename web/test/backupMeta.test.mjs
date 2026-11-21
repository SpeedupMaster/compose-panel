import test from 'node:test'
import assert from 'node:assert/strict'
import { inferUploadMeta, isEngineCompatible } from '../src/components/backupMeta.ts'

test('inferUploadMeta detects database, volume, dir, and panel filenames', () => {
  assert.deepEqual(inferUploadMeta('pg-main-20261120.pgdump.zst.age'), {
    kind: 'database',
    format: 'pgdump',
    encrypted: true,
    label: 'pg-main-20261120',
  })
  assert.deepEqual(inferUploadMeta('volume_uploads_20261120.tar.zst'), {
    kind: 'volume',
    format: 'tar',
    encrypted: false,
    label: 'volume_uploads_20261120',
  })
  assert.deepEqual(inferUploadMeta('panel-20261120-030000.db'), {
    kind: 'panel',
    format: 'sqlite',
    encrypted: false,
    label: 'panel',
  })
  assert.deepEqual(inferUploadMeta('dump.sql.gz'), {
    kind: 'database',
    format: 'sql',
    encrypted: false,
    label: 'dump',
  })
  assert.deepEqual(inferUploadMeta('redis.rdb.zst.age'), {
    kind: 'database',
    format: 'rdb',
    encrypted: true,
    label: 'redis',
  })
  assert.deepEqual(inferUploadMeta('site-assets.tgz'), {
    kind: 'dir',
    format: 'tar',
    encrypted: false,
    label: 'site-assets',
  })
})

test('isEngineCompatible enforces backup format to DB engine rules', () => {
  assert.equal(isEngineCompatible('pgdump', 'postgres'), true)
  assert.equal(isEngineCompatible('pgdump', 'mysql'), false)
  assert.equal(isEngineCompatible('sql', 'postgres'), true)
  assert.equal(isEngineCompatible('sql', 'mysql'), true)
  assert.equal(isEngineCompatible('sql', 'mariadb'), true)
  assert.equal(isEngineCompatible('sql', 'mongo'), false)
  assert.equal(isEngineCompatible('archive', 'mongo'), true)
  assert.equal(isEngineCompatible('rdb', 'redis'), true)
  assert.equal(isEngineCompatible('rdb', 'postgres'), false)
})
