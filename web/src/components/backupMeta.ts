export type BackupKind = 'database' | 'volume' | 'dir' | 'panel'

export type InferredUploadMeta = {
  kind: BackupKind
  format: string
  encrypted: boolean
  label: string
}

export const kindLabels: Record<BackupKind, string> = {
  database: '数据库',
  volume: '数据卷',
  dir: '项目目录',
  panel: '面板数据',
}

export const formatLabels: Record<string, string> = {
  pgdump: 'PostgreSQL (pg_dump)',
  sql: 'SQL 文本 (.sql)',
  archive: 'MongoDB (.archive)',
  rdb: 'Redis (.rdb)',
  tar: '归档 (.tar)',
  sqlite: '面板数据库 (.db / .sqlite)',
}

export function inferUploadMeta(filename: string): InferredUploadMeta {
  const base = filename.split(/[/\\]/).pop() || filename
  let name = base
  let lower = name.toLowerCase()
  let encrypted = false
  if (lower.endsWith('.age')) {
    encrypted = true
    name = name.slice(0, -4)
    lower = name.toLowerCase()
  }
  if (lower.endsWith('.zst')) {
    name = name.slice(0, -4)
  } else if (lower.endsWith('.tgz')) {
    name = name.slice(0, -4) + '.tar'
  } else if (lower.endsWith('.gz')) {
    name = name.slice(0, -3)
  }
  lower = name.toLowerCase()
  const dot = lower.lastIndexOf('.')
  const ext = dot >= 0 ? lower.slice(dot) : ''
  const stem = dot >= 0 ? name.slice(0, dot) : name

  let kind: BackupKind = 'database'
  let format = ''
  switch (ext) {
    case '.pgdump':
    case '.dump':
    case '.backup':
      kind = 'database'
      format = 'pgdump'
      break
    case '.sql':
      kind = 'database'
      format = 'sql'
      break
    case '.archive':
      kind = 'database'
      format = 'archive'
      break
    case '.rdb':
      kind = 'database'
      format = 'rdb'
      break
    case '.sqlite':
    case '.db':
    case '.sqlite3':
      kind = 'panel'
      format = 'sqlite'
      break
    case '.tar':
      format = 'tar'
      kind = lower.startsWith('volume_') ? 'volume' : 'dir'
      break
  }
  const label = kind === 'panel' ? 'panel' : stem || base
  return { kind, format, encrypted, label }
}

export function isEngineCompatible(format: string, engine: string): boolean {
  switch (format) {
    case '':
    case 'any':
      return true
    case 'pgdump':
      return engine === 'postgres'
    case 'sql':
      return engine === 'postgres' || engine === 'mysql' || engine === 'mariadb'
    case 'archive':
      return engine === 'mongo'
    case 'rdb':
      return engine === 'redis'
    default:
      return false
  }
}
