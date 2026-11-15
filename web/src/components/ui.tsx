import { createContext, ReactNode, useCallback, useContext, useEffect, useRef, useState } from 'react'
import { ApiError, get, post, streamLines } from '../api'

// ---------- status badges ----------

const statusMap: Record<string, [string, string]> = {
  running: ['ok', '运行中'],
  healthy: ['ok', '健康'],
  success: ['ok', '成功'],
  ok: ['ok', '正常'],
  partial: ['warn', '部分运行'],
  restarting: ['warn', '重启中'],
  rolled_back: ['warn', '已回滚'],
  starting: ['run', '启动中'],
  queued: ['off', '排队中'],
  pending: ['off', '等待中'],
  stopped: ['off', '已停止'],
  exited: ['off', '已退出'],
  created: ['off', '已创建'],
  paused: ['off', '已暂停'],
  empty: ['off', '未部署'],
  canceled: ['off', '已取消'],
  cancelled: ['off', '已取消'],
  disabled: ['off', '已停用'],
  missing: ['off', '不存在'],
  error: ['err', '异常'],
  failed: ['err', '失败'],
  unhealthy: ['err', '不健康'],
  dead: ['err', '已终止'],
}

export function Status({ s, label }: { s: string; label?: string }) {
  const [cls, text] = statusMap[s] || ['off', s || '—']
  return (
    <span className={'badge ' + cls}>
      <span className="dot" />
      {label ?? text}
    </span>
  )
}

export function JobStatus({ s }: { s: string }) {
  if (s === 'running') return <span className="badge run"><span className="dot" />进行中</span>
  return <Status s={s} />
}

export function Badge({ kind = 'off', children }: { kind?: string; children: ReactNode }) {
  return <span className={'badge ' + kind}>{children}</span>
}

// ---------- page frame ----------

export function Page({ title, crumbs, actions, children, sub }: { title: ReactNode; crumbs?: ReactNode; actions?: ReactNode; sub?: ReactNode; children: ReactNode }) {
  useEffect(() => {
    if (typeof title === 'string') document.title = title + ' · Compose 面板'
  }, [title])
  return (
    <>
      <div className="page-head">
        <div style={{ minWidth: 0 }}>
          {crumbs && <div className="crumbs">{crumbs}</div>}
          <h1 className="h1">{title}</h1>
          {sub && <div className="muted" style={{ marginTop: 4 }}>{sub}</div>}
        </div>
        {actions && <div className="row wrap">{actions}</div>}
      </div>
      <div className="page-body">{children}</div>
    </>
  )
}

export function Card({ title, actions, children, pad = true }: { title?: ReactNode; actions?: ReactNode; children: ReactNode; pad?: boolean }) {
  return (
    <section className="card">
      {(title || actions) && (
        <div className="card-head">
          <h2 className="st">{title}</h2>
          {actions && <div className="row wrap">{actions}</div>}
        </div>
      )}
      {pad ? <div className="card-body">{children}</div> : children}
    </section>
  )
}

export function Empty({ children, action }: { children: ReactNode; action?: ReactNode }) {
  return (
    <div className="empty">
      <div>{children}</div>
      {action}
    </div>
  )
}

export function Loading() {
  return <div className="empty">加载中…</div>
}

export function ErrorBox({ error }: { error: unknown }) {
  if (!error) return null
  return <div className="alert err">{error instanceof Error ? error.message : String(error)}</div>
}

// ---------- data loading hook ----------

export function useLoad<T>(fn: () => Promise<T>, deps: unknown[] = [], intervalMs = 0) {
  const [data, setData] = useState<T | undefined>()
  const [error, setError] = useState<unknown>()
  const [loading, setLoading] = useState(true)
  const fnRef = useRef(fn)
  fnRef.current = fn
  const reload = useCallback(async () => {
    try {
      const d = await fnRef.current()
      setData(d)
      setError(undefined)
    } catch (e) {
      setError(e)
    } finally {
      setLoading(false)
    }
  }, [])
  useEffect(() => {
    setLoading(true)
    reload()
    if (intervalMs > 0) {
      const t = setInterval(() => {
        if (!document.hidden) reload()
      }, intervalMs)
      return () => clearInterval(t)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps)
  return { data, error, loading, reload, setData }
}

// ---------- modal ----------

export function Modal({ title, onClose, children, footer, wide }: { title: ReactNode; onClose: () => void; children: ReactNode; footer?: ReactNode; wide?: boolean }) {
  useEffect(() => {
    const h = (e: KeyboardEvent) => e.key === 'Escape' && onClose()
    window.addEventListener('keydown', h)
    return () => window.removeEventListener('keydown', h)
  }, [onClose])
  return (
    <div className="overlay" onMouseDown={(e) => e.target === e.currentTarget && onClose()}>
      <div className={'modal' + (wide ? ' wide' : '')} role="dialog" aria-modal="true">
        <div className="modal-head">
          <h2 className="st">{title}</h2>
          <button className="btn btn-ghost btn-sm btn-icon" aria-label="关闭" onClick={onClose}>
            <Icon name="x" />
          </button>
        </div>
        <div className="modal-body">{children}</div>
        {footer && <div className="modal-foot">{footer}</div>}
      </div>
    </div>
  )
}

// Confirmation dialog; when `typeToConfirm` is set the user must type it.
export function Confirm({ title, children, confirmText = '确认', danger, typeToConfirm, onConfirm, onClose }: {
  title: string
  children?: ReactNode
  confirmText?: string
  danger?: boolean
  typeToConfirm?: string
  onConfirm: () => Promise<unknown> | void
  onClose: () => void
}) {
  const [typed, setTyped] = useState('')
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState<unknown>()
  const run = async () => {
    setBusy(true)
    setErr(undefined)
    try {
      await onConfirm()
      onClose()
    } catch (e) {
      setErr(e)
    } finally {
      setBusy(false)
    }
  }
  return (
    <Modal
      title={title}
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose}>取消</button>
          <button className={danger ? 'btn btn-danger solid' : 'btn btn-primary'} disabled={busy || (!!typeToConfirm && typed !== typeToConfirm)} onClick={run}>
            {busy ? '处理中…' : confirmText}
          </button>
        </>
      }
    >
      {children}
      {typeToConfirm && (
        <label className="field">
          <span>输入 <code className="mono">{typeToConfirm}</code> 以确认</span>
          <input className="input" value={typed} onChange={(e) => setTyped(e.target.value)} autoFocus />
        </label>
      )}
      <ErrorBox error={err} />
    </Modal>
  )
}

// ---------- toasts ----------

type ToastItem = { id: number; kind: 'ok' | 'err' | 'info'; text: string }
const ToastCtx = createContext<(kind: ToastItem['kind'], text: string) => void>(() => {})

export function ToastProvider({ children }: { children: ReactNode }) {
  const [items, setItems] = useState<ToastItem[]>([])
  const push = useCallback((kind: ToastItem['kind'], text: string) => {
    const id = Date.now() + Math.random()
    setItems((x) => [...x, { id, kind, text }])
    setTimeout(() => setItems((x) => x.filter((i) => i.id !== id)), kind === 'err' ? 8000 : 4000)
  }, [])
  return (
    <ToastCtx.Provider value={push}>
      {children}
      <div className="toast-wrap" aria-live="polite">
        {items.map((t) => (
          <div key={t.id} className={'toast ' + (t.kind === 'info' ? '' : t.kind)}>
            <div className="grow">{t.text}</div>
            <button className="link-btn" style={{ color: '#fff' }} aria-label="关闭" onClick={() => setItems((x) => x.filter((i) => i.id !== t.id))}>
              <Icon name="x" size={14} />
            </button>
          </div>
        ))}
      </div>
    </ToastCtx.Provider>
  )
}

export function useToast() {
  const push = useContext(ToastCtx)
  return {
    ok: (t: string) => push('ok', t),
    err: (e: unknown) => push('err', e instanceof ApiError || e instanceof Error ? e.message : String(e)),
    info: (t: string) => push('info', t),
  }
}

// Runs an async action with busy state and toasts.
export function useAction() {
  const toast = useToast()
  const [busy, setBusy] = useState<string | null>(null)
  const run = async <T,>(key: string, fn: () => Promise<T>, okText?: string): Promise<T | undefined> => {
    setBusy(key)
    try {
      const r = await fn()
      if (okText) toast.ok(okText)
      const w = (r as any)?.warning
      if (w) toast.err(w)
      return r
    } catch (e) {
      toast.err(e)
      return undefined
    } finally {
      setBusy(null)
    }
  }
  return { busy, run }
}

// ---------- job log drawer ----------

const JobCtx = createContext<(id: string, title?: string) => void>(() => {})
export const useJobLog = () => useContext(JobCtx)

export function JobLogProvider({ children }: { children: ReactNode }) {
  const [job, setJob] = useState<{ id: string; title?: string } | null>(null)
  const open = useCallback((id: string, title?: string) => setJob({ id, title }), [])
  return (
    <JobCtx.Provider value={open}>
      {children}
      {job && <JobDrawer id={job.id} title={job.title} onClose={() => setJob(null)} />}
    </JobCtx.Provider>
  )
}

export function lineClass(l: string) {
  if (/^(==>|▶|\[step\])/.test(l)) return 'ln-step'
  if (/^\$ /.test(l)) return 'ln-cmd'
  if (/(error|错误|失败|failed|fatal)/i.test(l)) return 'ln-err'
  if (/^(✓|完成|成功)/.test(l)) return 'ln-ok'
  return ''
}

export function LogView({ lines, height = 420, follow = true, filter }: { lines: string[]; height?: number | string; follow?: boolean; filter?: string }) {
  const ref = useRef<HTMLDivElement>(null)
  useEffect(() => {
    if (follow && ref.current) ref.current.scrollTop = ref.current.scrollHeight
  }, [lines, follow])
  const shown = filter ? lines.filter((l) => l.toLowerCase().includes(filter.toLowerCase())) : lines
  return (
    <div ref={ref} className="log" style={{ height }}>
      {shown.length === 0 ? <span style={{ color: '#888' }}>（暂无输出）</span> : shown.map((l, i) => <div key={i} className={lineClass(l)}>{l || ' '}</div>)}
    </div>
  )
}

function JobDrawer({ id, title, onClose }: { id: string; title?: string; onClose: () => void }) {
  const [lines, setLines] = useState<string[]>([])
  const [status, setStatus] = useState('running')
  const [job, setJob] = useState<any>()
  useEffect(() => {
    setLines([])
    const buf: string[] = []
    let raf = 0
    const flush = () => {
      raf = 0
      setLines([...buf])
    }
    const close = streamLines(`/jobs/${id}/log`, (l) => {
      buf.push(l)
      if (!raf) raf = requestAnimationFrame(flush)
    }, (st) => {
      setStatus(st || 'done')
      get(`/jobs/${id}`).then(setJob).catch(() => {})
    })
    get(`/jobs/${id}`).then(setJob).catch(() => {})
    return () => {
      close()
      if (raf) cancelAnimationFrame(raf)
    }
  }, [id])
  const cancel = async () => {
    await post(`/jobs/${id}/cancel`).catch(() => {})
  }
  const st = job?.status && job.status !== 'running' ? job.status : status === 'running' ? 'running' : job?.status || status
  return (
    <>
      <div className="overlay" style={{ background: 'rgba(24,25,27,.25)' }} onMouseDown={onClose} />
      <aside className="drawer" role="dialog" aria-label="任务日志">
        <div className="modal-head">
          <div className="grow">
            <div className="st ellipsis">{title || job?.title || '任务日志'}</div>
            <div className="label mono">{id}</div>
          </div>
          <JobStatus s={st} />
          {st === 'running' && <button className="btn btn-sm" onClick={cancel}>取消任务</button>}
          <button className="btn btn-ghost btn-sm btn-icon" aria-label="关闭" onClick={onClose}><Icon name="x" /></button>
        </div>
        {job?.error && <div className="alert err" style={{ margin: 12 }}>{job.error}</div>}
        <LogView lines={lines} height="100%" />
      </aside>
    </>
  )
}

// ---------- misc ----------

export function SecretBox({ value, label }: { value: string; label?: string }) {
  const [copied, setCopied] = useState(false)
  return (
    <div className="col" style={{ gap: 6 }}>
      {label && <span className="label">{label}</span>}
      <div className="secret-box">
        <code>{value}</code>
        <button className="btn btn-sm" onClick={() => copy(value).then(() => { setCopied(true); setTimeout(() => setCopied(false), 1500) })}>
          {copied ? '已复制' : '复制'}
        </button>
      </div>
    </div>
  )
}

export async function copy(text: string) {
  try {
    await navigator.clipboard.writeText(text)
  } catch {
    const ta = document.createElement('textarea')
    ta.value = text
    document.body.appendChild(ta)
    ta.select()
    document.execCommand('copy')
    ta.remove()
  }
}

export function Field({ label, hint, children }: { label: ReactNode; hint?: ReactNode; children: ReactNode }) {
  return (
    <label className="field">
      <span>{label}</span>
      {children}
      {hint && <span className="hint">{hint}</span>}
    </label>
  )
}

export function Tabs<T extends string>({ tabs, value, onChange }: { tabs: [T, ReactNode][]; value: T; onChange: (v: T) => void }) {
  return (
    <div className="tabs" role="tablist">
      {tabs.map(([k, l]) => (
        <button key={k} role="tab" aria-selected={k === value} className={k === value ? 'on' : ''} onClick={() => onChange(k)}>
          {l}
        </button>
      ))}
    </div>
  )
}

export function Meter({ value, max, warnAt = 75, errAt = 90 }: { value: number; max: number; warnAt?: number; errAt?: number }) {
  const p = max ? Math.min(100, (value / max) * 100) : 0
  return (
    <div className={'track' + (p >= errAt ? ' err' : p >= warnAt ? ' warn' : '')} role="meter" aria-valuenow={Math.round(p)} aria-valuemin={0} aria-valuemax={100}>
      <div style={{ width: p + '%' }} />
    </div>
  )
}

// ---------- icons (stroke, 18px) ----------

const paths: Record<string, string> = {
  overview: 'M3 3h7v9H3zM14 3h7v5h-7zM14 12h7v9h-7zM3 16h7v5H3z',
  projects: 'M3 7l9-4 9 4-9 4-9-4zM3 12l9 4 9-4M3 17l9 4 9-4',
  docker: 'M4 10h3v3H4zM8 10h3v3H8zM12 10h3v3h-3zM8 6h3v3H8zM12 6h3v3h-3zM3 14h17c-1 4-5 6-9 6-4 0-7-2-8-6z',
  databases: 'M4 6c0-1.7 3.6-3 8-3s8 1.3 8 3-3.6 3-8 3-8-1.3-8-3zM4 6v12c0 1.7 3.6 3 8 3s8-1.3 8-3V6M4 12c0 1.7 3.6 3 8 3s8-1.3 8-3',
  backups: 'M12 3v12M7 10l5 5 5-5M4 17v3h16v-3',
  sites: 'M12 3a9 9 0 100 18 9 9 0 000-18zM3 12h18M12 3c2.5 2.5 3.8 5.5 3.8 9s-1.3 6.5-3.8 9c-2.5-2.5-3.8-5.5-3.8-9S9.5 5.5 12 3z',
  certs: 'M12 3l7 3v5c0 5-3 8.5-7 10-4-1.5-7-5-7-10V6l7-3zM9 12l2 2 4-4',
  jobs: 'M4 6h16M4 12h10M4 18h7M17 15l2 2 3-4',
  integrations: 'M9 7V3M15 7V3M7 7h10v4a5 5 0 01-10 0V7zM12 16v5',
  access: 'M9 11a4 4 0 100-8 4 4 0 000 8zM2 21c0-3.9 3.1-7 7-7s7 3.1 7 7M17 11l2 2 4-4',
  settings: 'M12 15a3 3 0 100-6 3 3 0 000 6zM19.4 15a1.7 1.7 0 00.3 1.8l.1.1a2 2 0 11-2.8 2.8l-.1-.1a1.7 1.7 0 00-1.8-.3 1.7 1.7 0 00-1 1.5V21a2 2 0 11-4 0v-.1a1.7 1.7 0 00-1.1-1.5 1.7 1.7 0 00-1.8.3l-.1.1a2 2 0 11-2.8-2.8l.1-.1a1.7 1.7 0 00.3-1.8 1.7 1.7 0 00-1.5-1H3a2 2 0 110-4h.1a1.7 1.7 0 001.5-1.1 1.7 1.7 0 00-.3-1.8l-.1-.1a2 2 0 112.8-2.8l.1.1a1.7 1.7 0 001.8.3H9a1.7 1.7 0 001-1.5V3a2 2 0 114 0v.1a1.7 1.7 0 001 1.5 1.7 1.7 0 001.8-.3l.1-.1a2 2 0 112.8 2.8l-.1.1a1.7 1.7 0 00-.3 1.8V9a1.7 1.7 0 001.5 1H21a2 2 0 110 4h-.1a1.7 1.7 0 00-1.5 1z',
  logout: 'M9 21H5a2 2 0 01-2-2V5a2 2 0 012-2h4M16 17l5-5-5-5M21 12H9',
  x: 'M18 6L6 18M6 6l12 12',
  plus: 'M12 5v14M5 12h14',
  menu: 'M3 6h18M3 12h18M3 18h18',
  play: 'M6 4l14 8-14 8V4z',
  stop: 'M6 6h12v12H6z',
  restart: 'M3 12a9 9 0 1015-6.7L21 8M21 3v5h-5',
  rocket: 'M5 15c-1.5 1.3-2 5-2 5s3.7-.5 5-2c.7-.8.7-2.1-.1-2.9a2.1 2.1 0 00-2.9-.1zM12 15l-3-3a22 22 0 012-4A12.9 12.9 0 0122 2c0 2.7-.8 7.5-6 11a22.4 22.4 0 01-4 2z',
  terminal: 'M4 17l6-6-6-6M12 19h8',
  logs: 'M14 3H6a2 2 0 00-2 2v14a2 2 0 002 2h12a2 2 0 002-2V9l-6-6zM14 3v6h6M8 13h8M8 17h5',
  trash: 'M3 6h18M8 6V4h8v2M19 6l-1 14H6L5 6',
  download: 'M12 3v12M7 10l5 5 5-5M4 21h16',
  refresh: 'M21 12a9 9 0 11-3-6.7L21 8M21 3v5h-5',
  check: 'M5 12l5 5L20 7',
  lock: 'M5 11h14v10H5zM8 11V7a4 4 0 018 0v4',
  key: 'M15 7a4 4 0 11-4 4l-8 8v-3l2-2v-2h2l2-2M17 9h.01',
  copy: 'M9 9h11v11H9zM5 15H4V4h11v1',
  edit: 'M12 20h9M16.5 3.5a2.1 2.1 0 013 3L7 19l-4 1 1-4 12.5-12.5z',
  search: 'M11 19a8 8 0 100-16 8 8 0 000 16zM21 21l-4.3-4.3',
  alert: 'M12 9v4M12 17h.01M10.3 3.9L1.8 18a2 2 0 001.7 3h17a2 2 0 001.7-3L13.7 3.9a2 2 0 00-3.4 0z',
  git: 'M6 3v12M18 9a3 3 0 100-6 3 3 0 000 6zM6 21a3 3 0 100-6 3 3 0 000 6zM18 9a9 9 0 01-9 9',
  folder: 'M3 6a2 2 0 012-2h4l2 2h8a2 2 0 012 2v10a2 2 0 01-2 2H5a2 2 0 01-2-2V6z',
  file: 'M14 3H6a2 2 0 00-2 2v14a2 2 0 002 2h12a2 2 0 002-2V9l-6-6zM14 3v6h6',
  upload: 'M12 21V9M7 14l5-5 5 5M4 3h16',
  chart: 'M3 3v18h18M7 15l4-4 3 3 6-6',
  clock: 'M12 21a9 9 0 100-18 9 9 0 000 18zM12 7v5l3 2',
  link: 'M10 14a5 5 0 007 0l3-3a5 5 0 00-7-7l-1 1M14 10a5 5 0 00-7 0l-3 3a5 5 0 007 7l1-1',
  external: 'M18 13v6a2 2 0 01-2 2H5a2 2 0 01-2-2V8a2 2 0 012-2h6M15 3h6v6M10 14L21 3',
}

export function Icon({ name, size = 18 }: { name: string; size?: number }) {
  return (
    <svg width={size} height={size} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={1.8} strokeLinecap="round" strokeLinejoin="round" aria-hidden="true" style={{ flex: 'none' }}>
      <path d={paths[name] || ''} />
    </svg>
  )
}
