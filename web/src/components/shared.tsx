import { Field } from './ui'

export function triggerName(t: string) {
  return ({
    manual: '手动',
    webhook: 'Webhook',
    rollback: '回滚',
    create: '创建',
    api: 'API',
    import: '导入',
    image_update: '镜像自动更新',
  } as Record<string, string>)[t] || t
}

export function sourceName(s: string) {
  return ({ inline: '面板编辑', git: 'Git 仓库', imported: '导入', database: '数据库' } as Record<string, string>)[s] || s
}

export function portsText(ports: any[] | null) {
  if (!ports?.length) return '—'
  const seen = new Set<string>()
  for (const p of ports) seen.add(p.PublicPort ? `${p.PublicPort}→${p.PrivatePort}` : `${p.PrivatePort}/${p.Type}`)
  return [...seen].join(' ')
}

export const exposureName: Record<string, string> = {
  internal: '仅内部网络',
  local: '本机 127.0.0.1',
  public: '公网',
}

export function engineLabel(id: string) {
  return ({
    postgres: 'PostgreSQL',
    mysql: 'MySQL',
    mariadb: 'MariaDB',
    mongo: 'MongoDB',
    redis: 'Redis',
  } as Record<string, string>)[id] || id
}

// Mirrors dbmgr.PostgresMajorFromImage: postgres:18-alpine, pgvector/pgvector:pg17,
// timescale/timescaledb:2.17.2-pg16 → major version; '' when the tag doesn't say.
export function pgMajorFromImage(image: string) {
  const ref = image.trim().split('@')[0]
  const i = ref.lastIndexOf(':')
  if (i < 0 || i < ref.lastIndexOf('/')) return ''
  const tag = ref.slice(i + 1)
  return (tag.match(/pg-?(\d{2})(?:\D|$)/i) || tag.match(/^(\d{2})(?:\D|$)/) || [])[1] || ''
}

export const defaultImage = (eng: any, version: string) => (eng?.image || '').replace('{v}', version)

// Example of a compatible variant, shown under the image field.
function imageExample(engine: string, version: string) {
  const v = version.split('.')[0]
  switch (engine) {
    case 'postgres':
      return `pgvector/pgvector:pg${v}、postgis/postgis:${v}-3.5`
    case 'redis':
      return 'valkey/valkey:8-alpine'
    default:
      return `registry.example.com/${engine}:${version}`
  }
}

export function ImageField({ engine, value, placeholder, version, onChange, readOnly, className }: {
  engine: string
  value: string
  placeholder: string
  version: string
  onChange: (v: string) => void
  readOnly?: boolean
  className?: string
}) {
  const m = engine === 'postgres' ? pgMajorFromImage(value) : ''
  return (
    <Field
      label="镜像（可选）"
      className={className}
      hint={
        m && m !== version.split('.')[0] ? (
          <span style={{ color: '#A41E14' }}>该镜像是 PostgreSQL {m}，与版本 {version} 不一致</span>
        ) : (
          <>留空用官方镜像；也可填兼容变体，如 <span className="mono">{imageExample(engine, version)}</span></>
        )
      }
    >
      <input
        className="input mono"
        value={value}
        readOnly={readOnly}
        placeholder={placeholder}
        spellCheck={false}
        autoComplete="off"
        onChange={(e) => onChange(e.target.value.trim())}
      />
    </Field>
  )
}
