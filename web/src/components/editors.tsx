import { useEffect, useRef, useState } from 'react'
import type { CodeLang, EditorHandle } from './codemirror'
import { post } from '../api'
import { Icon } from './ui'

export type EnvVar = { key: string; value: string; secret: boolean }

// Code editor (CodeMirror, loaded on demand) with syntax highlighting, line numbers,
// folding, search (Ctrl+F) and Tab indentation. Until it loads, a plain textarea stands in.
export function CodeArea({ value, onChange, onSubmit, rows = 22, readOnly, placeholder, label, lang = 'yaml' }: { value: string; onChange?: (v: string) => void; onSubmit?: () => void; rows?: number; readOnly?: boolean; placeholder?: string; label?: string; lang?: CodeLang }) {
  const host = useRef<HTMLDivElement>(null)
  const ed = useRef<EditorHandle | null>(null)
  const [ready, setReady] = useState(false)
  const latest = useRef({ value, onChange, onSubmit, readOnly: !!readOnly || !onChange })
  latest.current = { value, onChange, onSubmit, readOnly: !!readOnly || !onChange }

  useEffect(() => {
    let dead = false
    import('./codemirror').then(({ createEditor }) => {
      if (dead || !host.current) return
      ed.current = createEditor(host.current, {
        value: latest.current.value,
        lang,
        readOnly: latest.current.readOnly,
        placeholder,
        label,
        onChange: (v) => latest.current.onChange?.(v),
        onSubmit: onSubmit ? () => latest.current.onSubmit?.() : undefined,
      })
      setReady(true)
    })
    return () => {
      dead = true
      ed.current?.destroy()
      ed.current = null
      setReady(false)
    }
    // The editor is rebuilt only when its language changes; value/readOnly sync below.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [lang])
  useEffect(() => { ed.current?.setValue(value) }, [value, ready])
  useEffect(() => { ed.current?.setReadOnly(!!readOnly || !onChange) }, [readOnly, !onChange, ready])

  return (
    <div className={'code-area' + (readOnly ? ' readonly' : '')} style={{ height: Math.round(rows * 20.8 + 18) }}>
      <div ref={host} className="code-host" />
      {!ready && <textarea className="code-fallback code" aria-label={label} spellCheck={false} value={value} readOnly={readOnly} placeholder={placeholder} onChange={(e) => onChange?.(e.target.value)} />}
    </div>
  )
}

export function EnvEditor({ env, onChange, readOnly }: { env: EnvVar[]; onChange: (e: EnvVar[]) => void; readOnly?: boolean }) {
  const [bulk, setBulk] = useState<string | null>(null)
  const set = (i: number, patch: Partial<EnvVar>) => onChange(env.map((e, k) => (k === i ? { ...e, ...patch } : e)))
  if (bulk !== null) {
    return (
      <div className="col">
        <CodeArea label="批量编辑环境变量" lang="env" value={bulk} onChange={setBulk} rows={12} placeholder={'KEY=value\n# 以 # 开头的行会被忽略'} />
        <div className="row">
          <button
            className="btn btn-primary btn-sm"
            onClick={() => {
              const secrets = new Set(env.filter((e) => e.secret).map((e) => e.key))
              const out: EnvVar[] = []
              for (const line of bulk.split(/\r?\n/)) {
                const t = line.trim()
                if (!t || t.startsWith('#')) continue
                const i = t.indexOf('=')
                if (i <= 0) continue
                const key = t.slice(0, i).trim().replace(/^export\s+/, '')
                let value = t.slice(i + 1)
                if (/^".*"$/.test(value) || /^'.*'$/.test(value)) value = value.slice(1, -1)
                out.push({ key, value, secret: secrets.has(key) || /(PASS|SECRET|TOKEN|KEY)/i.test(key) })
              }
              onChange(out)
              setBulk(null)
            }}
          >
            应用
          </button>
          <button className="btn btn-sm" onClick={() => setBulk(null)}>取消</button>
        </div>
      </div>
    )
  }
  return (
    <div className="col">
      {env.length === 0 && <div className="muted">没有环境变量。Compose 中用 <code className="mono">${'{'}KEY{'}'}</code> 引用。</div>}
      {env.map((e, i) => (
        <div className="row" key={i}>
          <input className="input mono" style={{ maxWidth: 240 }} aria-label="变量名" placeholder="KEY" value={e.key} readOnly={readOnly} onChange={(x) => set(i, { key: x.target.value })} />
          <input
            className="input mono grow"
            aria-label="变量值"
            placeholder={e.secret ? '（留空保持原值）' : 'value'}
            type={e.secret ? 'password' : 'text'}
            value={e.value}
            readOnly={readOnly}
            onChange={(x) => set(i, { value: x.target.value })}
            autoComplete="off"
          />
          <label className="check nowrap" title="密钥值加密保存，日志与界面中隐藏">
            <input type="checkbox" checked={e.secret} disabled={readOnly} onChange={(x) => set(i, { secret: x.target.checked })} />密钥
          </label>
          {!readOnly && (
            <button className="btn btn-ghost btn-sm btn-icon" aria-label={'删除 ' + e.key} onClick={() => onChange(env.filter((_, k) => k !== i))}>
              <Icon name="trash" size={16} />
            </button>
          )}
        </div>
      ))}
      {!readOnly && (
        <div className="row">
          <button className="btn btn-sm" onClick={() => onChange([...env, { key: '', value: '', secret: false }])}><Icon name="plus" size={16} />添加变量</button>
          <button className="btn btn-sm btn-ghost" onClick={() => setBulk(env.map((e) => `${e.key}=${e.secret ? '' : e.value}`).join('\n'))}>批量编辑</button>
        </div>
      )}
    </div>
  )
}

export type SiteSuggestion = { service: string; port: number; portFrom: '' | 'label' | 'compose' | 'image' | 'known'; domains: string[]; ports: Record<string, number[]>; portsFrom: Record<string, string> }
export type Validation = { valid: boolean; error?: string; services?: string[]; warnings: string[]; site?: SiteSuggestion | null }

export async function validateCompose(compose: string, env: EnvVar[], name: string) {
  return post<Validation>('/compose/validate', { compose, env, name })
}

export function ValidationResult({ v }: { v: Validation | null }) {
  if (!v) return null
  return (
    <div className="col">
      {v.valid ? (
        <div className="alert ok"><Icon name="check" /><div>配置有效 · 服务：<span className="mono">{v.services?.join(', ') || '—'}</span></div></div>
      ) : (
        <div className="alert err"><Icon name="alert" /><pre className="mono" style={{ margin: 0, whiteSpace: 'pre-wrap' }}>{v.error}</pre></div>
      )}
      {v.warnings?.map((w, i) => (
        <div key={i} className="alert warn"><Icon name="alert" /><div>{w}</div></div>
      ))}
    </div>
  )
}

// Line diff (LCS) for small files.
export function diffLines(a: string, b: string): { t: ' ' | '+' | '-'; s: string }[] {
  const x = a.split('\n')
  const y = b.split('\n')
  const n = x.length
  const m = y.length
  if (n * m > 4_000_000) return [...x.map((s) => ({ t: '-' as const, s })), ...y.map((s) => ({ t: '+' as const, s }))]
  const dp: Uint32Array[] = Array.from({ length: n + 1 }, () => new Uint32Array(m + 1))
  for (let i = n - 1; i >= 0; i--) for (let j = m - 1; j >= 0; j--) dp[i][j] = x[i] === y[j] ? dp[i + 1][j + 1] + 1 : Math.max(dp[i + 1][j], dp[i][j + 1])
  const out: { t: ' ' | '+' | '-'; s: string }[] = []
  let i = 0
  let j = 0
  while (i < n && j < m) {
    if (x[i] === y[j]) {
      out.push({ t: ' ', s: x[i] })
      i++
      j++
    } else if (dp[i + 1][j] >= dp[i][j + 1]) out.push({ t: '-', s: x[i++] })
    else out.push({ t: '+', s: y[j++] })
  }
  while (i < n) out.push({ t: '-', s: x[i++] })
  while (j < m) out.push({ t: '+', s: y[j++] })
  return out
}

export function Diff({ a, b }: { a: string; b: string }) {
  const d = diffLines(a, b)
  if (a === b) return <div className="muted" style={{ padding: 16 }}>与上一次部署相比没有变化。</div>
  return (
    <div className="diff" style={{ maxHeight: 520 }}>
      {d.map((l, i) => (
        <div key={i} className={l.t === '+' ? 'add' : l.t === '-' ? 'del' : ''}>{l.t} {l.s}</div>
      ))}
    </div>
  )
}

// Picks a language from a file name (for a project's extra files).
export function langForPath(path: string): CodeLang {
  const p = path.toLowerCase()
  const base = p.slice(p.lastIndexOf('/') + 1)
  if (/\.ya?ml$/.test(p)) return 'yaml'
  if (/\.json$/.test(p)) return 'json'
  if (/\.sql$/.test(p)) return 'sql'
  if (/\.toml$/.test(p)) return 'toml'
  if (/\.(sh|bash)$/.test(p)) return 'shell'
  if (base === 'dockerfile' || base.endsWith('.dockerfile')) return 'dockerfile'
  if (base.startsWith('.env') || /\.(env|ini|properties)$/.test(p)) return 'env'
  if (/(^|\/)(nginx|conf\.d|sites-(enabled|available))\//.test(p) || /\.conf$/.test(p)) return 'nginx'
  return 'text'
}
