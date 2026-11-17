import { Children, createContext, CSSProperties, Fragment, isValidElement, KeyboardEvent as ReactKeyboardEvent, ReactNode, useCallback, useContext, useEffect, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import {
  ChartLine, Check, ChevronDown, ChevronLeft, ChevronRight, Clock, Container, Copy, Database, Download, ExternalLink, File, Folder, GitBranch, Globe,
  Hammer, HardDrive, HardDriveDownload, KeyRound, Layers, LayoutDashboard, LayoutTemplate, Link, ListChecks, Lock, LogOut, Menu, Pencil, Play, Plug, Plus,
  Package, RefreshCw, Rocket, RotateCcw, ScrollText, Search, Settings, ShieldCheck, Square, SquareTerminal, Trash2, TriangleAlert, Upload, Users, X,
  type LucideIcon,
} from 'lucide-react'
import { ApiError, get, post, streamLines } from '../api'
import { calcMenuPlacement, MenuPlacement } from './geometry'
import { useAppUpdated } from '../pwa'

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

export function Card({ title, actions, children, pad = true, className }: { title?: ReactNode; actions?: ReactNode; children: ReactNode; pad?: boolean; className?: string }) {
  return (
    <section className={'card' + (className ? ' ' + className : '')}>
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

export function Empty({ children, action, icon }: { children: ReactNode; action?: ReactNode; icon?: string }) {
  return (
    <div className="empty">
      {icon && <span className="empty-icon"><Icon name={icon} size={22} /></span>}
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

let scrollLockCount = 0
let savedBodyOverflow = ''

/** Reference-counted body scroll lock shared by modals, sheets, and mobile navigation drawer. */
export function lockBodyScroll() {
  if (scrollLockCount++ === 0) {
    savedBodyOverflow = document.body.style.overflow
    document.body.style.overflow = 'hidden'
  }
  return () => {
    if (scrollLockCount > 0 && --scrollLockCount === 0) {
      document.body.style.overflow = savedBodyOverflow
    }
  }
}

function getViewportBounds() {
  const docEl = document.documentElement
  const vv = window.visualViewport
  const width = docEl.clientWidth || window.innerWidth
  const height = vv && Math.abs(vv.scale - 1) < 0.02 ? vv.height : (docEl.clientHeight || window.innerHeight)
  return { width, height }
}

function useIsMobileSheet(query = '(max-width: 640px)') {
  const [matches, setMatches] = useState(() =>
    typeof window !== 'undefined' && typeof window.matchMedia === 'function' ? window.matchMedia(query).matches : false,
  )
  useEffect(() => {
    if (typeof window === 'undefined' || typeof window.matchMedia !== 'function') return
    const mq = window.matchMedia(query)
    const onChange = () => setMatches(mq.matches)
    onChange()
    mq.addEventListener('change', onChange)
    return () => mq.removeEventListener('change', onChange)
  }, [query])
  return matches
}

function useDropdownDismiss({
  open,
  floating,
  triggerRef,
  listRef,
  onClose,
  onReposition,
  returnFocusOnEsc = true,
}: {
  open: boolean
  floating: boolean
  triggerRef: { current: HTMLElement | null }
  listRef: { current: HTMLElement | null }
  onClose: () => void
  onReposition?: () => void
  returnFocusOnEsc?: boolean
}) {
  useEffect(() => {
    if (!open) return
    const onDown = (e: PointerEvent) => {
      const t = e.target as Node
      if (!triggerRef.current?.contains(t) && !listRef.current?.contains(t)) onClose()
    }
    const onEsc = (e: KeyboardEvent) => {
      if (e.key !== 'Escape') return
      e.preventDefault()
      e.stopImmediatePropagation()
      onClose()
      if (returnFocusOnEsc) triggerRef.current?.focus()
    }
    const onMove = (e: Event) => {
      if (listRef.current && e.target instanceof Node && listRef.current.contains(e.target)) return
      onReposition?.()
    }
    document.addEventListener('pointerdown', onDown)
    window.addEventListener('keydown', onEsc, true)
    if (floating && onReposition) {
      window.addEventListener('resize', onMove)
      window.addEventListener('scroll', onMove, true)
      window.visualViewport?.addEventListener('resize', onMove)
    }
    return () => {
      document.removeEventListener('pointerdown', onDown)
      window.removeEventListener('keydown', onEsc, true)
      window.removeEventListener('resize', onMove)
      window.removeEventListener('scroll', onMove, true)
      window.visualViewport?.removeEventListener('resize', onMove)
    }
  }, [open, floating, triggerRef, listRef, onClose, onReposition, returnFocusOnEsc])
}

// ---------- modal ----------

export function Modal({ title, onClose, children, footer, wide }: { title: ReactNode; onClose: () => void; children: ReactNode; footer?: ReactNode; wide?: boolean }) {
  useEffect(() => {
    const unlock = lockBodyScroll()
    const h = (e: KeyboardEvent) => e.key === 'Escape' && onClose()
    window.addEventListener('keydown', h)
    return () => {
      window.removeEventListener('keydown', h)
      unlock()
    }
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
  const appUpdated = useAppUpdated()
  const push = useCallback((kind: ToastItem['kind'], text: string) => {
    const id = Date.now() + Math.random()
    setItems((x) => [...x, { id, kind, text }])
    setTimeout(() => setItems((x) => x.filter((i) => i.id !== id)), kind === 'err' ? 8000 : 4000)
  }, [])
  return (
    <ToastCtx.Provider value={push}>
      {children}
      <div className="toast-wrap" aria-live="polite">
        {appUpdated && (
          <div className="toast">
            <div className="grow">面板已更新到新版本，刷新后生效。</div>
            <button className="link-btn" style={{ color: '#fff', fontWeight: 600 }} onClick={() => window.location.reload()}>
              刷新
            </button>
          </div>
        )}
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
    <div ref={ref} className="log" style={{ '--log-h': typeof height === 'number' ? `${height}px` : height } as CSSProperties}>
      {shown.length === 0 ? <span style={{ color: '#888' }}>（暂无输出）</span> : shown.map((l, i) => <div key={i} className={lineClass(l)}>{l || ' '}</div>)}
    </div>
  )
}

function JobDrawer({ id, title, onClose }: { id: string; title?: string; onClose: () => void }) {
  const [lines, setLines] = useState<string[]>([])
  const [status, setStatus] = useState('running')
  const [job, setJob] = useState<any>()
  useEffect(() => {
    const unlock = lockBodyScroll()
    const h = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
    }
    window.addEventListener('keydown', h)
    return () => {
      window.removeEventListener('keydown', h)
      unlock()
    }
  }, [onClose])
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
          <div className="grow" style={{ minWidth: 0 }}>
            <div className="st ellipsis">{title || job?.title || '任务日志'}</div>
            <div className="label mono ellipsis">{id}</div>
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

// div: render a <div> instead of <label>, for controls that are labels themselves (FilePicker).
export function Field({ label, hint, children, className, div }: { label: ReactNode; hint?: ReactNode; children: ReactNode; className?: string; div?: boolean }) {
  const Tag = div ? 'div' : 'label'
  return (
    <Tag className={'field' + (className ? ' ' + className : '')}>
      <span>{label}</span>
      {children}
      {hint && <span className="hint">{hint}</span>}
    </Tag>
  )
}

function fmtSize(n: number) {
  if (n < 1024) return n + ' B'
  const u = ['KB', 'MB', 'GB']
  let i = -1
  do { n /= 1024; i++ } while (n >= 1024 && i < u.length - 1)
  return (n < 10 ? n.toFixed(1) : Math.round(n)) + ' ' + u[i]
}

// Page numbers to show: first, last, and a window around the current page; 0 marks a gap.
function pageList(page: number, pages: number): number[] {
  const want = new Set([1, pages, page - 1, page, page + 1])
  if (page <= 3) [2, 3, 4].forEach((p) => want.add(p))
  if (page >= pages - 2) [pages - 1, pages - 2, pages - 3].forEach((p) => want.add(p))
  const list = [...want].filter((p) => p >= 1 && p <= pages).sort((a, b) => a - b)
  const out: number[] = []
  list.forEach((p, i) => {
    if (i > 0 && p - list[i - 1] === 2) out.push(p - 1) // a gap of one page: just show it
    else if (i > 0 && p - list[i - 1] > 2) out.push(0)
    out.push(p)
  })
  return out
}

// Table footer pager. page is 1-based.
export function Pager({ page, pageSize, total, onPage, onPageSize, sizes = [20, 50, 100] }: {
  page: number; pageSize: number; total: number; onPage: (p: number) => void; onPageSize?: (n: number) => void; sizes?: number[]
}) {
  const pages = Math.max(1, Math.ceil(total / pageSize))
  const from = total === 0 ? 0 : (page - 1) * pageSize + 1
  const to = Math.min(total, page * pageSize)
  return (
    <nav className="pager" aria-label="分页">
      <span className="label">共 {total} 条{total > 0 && <> · 第 {from}–{to} 条</>}</span>
      <div className="pager-ctrl">
        {onPageSize && (
          <Select className="sm" style={{ width: 104 }} aria-label="每页条数" value={String(pageSize)} onChange={(v) => onPageSize(Number(v))}>
            {sizes.map((n) => <option key={n} value={n}>{n} 条/页</option>)}
          </Select>
        )}
        <button className="btn btn-sm pager-btn" aria-label="上一页" disabled={page <= 1} onClick={() => onPage(page - 1)}><Icon name="chevron-left" size={16} /></button>
        {pageList(page, pages).map((p, i) => p === 0
          ? <span key={'gap' + i} className="pager-gap">…</span>
          : <button key={p} className={'btn btn-sm pager-btn' + (p === page ? ' on' : '')} aria-current={p === page ? 'page' : undefined} onClick={() => onPage(p)}>{p}</button>)}
        <button className="btn btn-sm pager-btn" aria-label="下一页" disabled={page >= pages} onClick={() => onPage(page + 1)}><Icon name="chevron-right" size={16} /></button>
      </div>
    </nav>
  )
}

// Click-or-drop file picker. Controlled by `file`: passing null clears the native input too.
export function FilePicker({ file, onChange, accept, hint, disabled, label = '文件' }: {
  file: File | null; onChange: (f: File | null) => void; accept?: string; hint?: ReactNode; disabled?: boolean; label?: string
}) {
  const ref = useRef<HTMLInputElement>(null)
  const [drag, setDrag] = useState(false)
  useEffect(() => { if (!file && ref.current) ref.current.value = '' }, [file])
  return (
    <label className={'file-drop' + (drag ? ' drag' : '') + (file ? ' has' : '') + (disabled ? ' disabled' : '')}
      onDragOver={(e) => { if (disabled) return; e.preventDefault(); setDrag(true) }}
      onDragLeave={() => setDrag(false)}
      onDrop={(e) => { e.preventDefault(); setDrag(false); const f = e.dataTransfer.files?.[0]; if (f && !disabled) onChange(f) }}>
      <input ref={ref} className="sr-only" type="file" accept={accept} disabled={disabled} aria-label={label}
        onChange={(e) => onChange(e.target.files?.[0] || null)} />
      <span className="file-ico"><Icon name={file ? 'file' : 'upload'} size={18} /></span>
      <span className="file-main">
        {file ? (
          <><span className="file-title mono" title={file.name}>{file.name}</span><span className="label">{fmtSize(file.size)} · 点击更换</span></>
        ) : (
          <><span className="file-title"><em>选择文件</em> 或拖放到这里</span>{hint && <span className="label">{hint}</span>}</>
        )}
      </span>
      {file && !disabled && (
        <button type="button" className="btn btn-sm" onClick={(e) => { e.preventDefault(); onChange(null) }}><Icon name="x" size={14} />移除</button>
      )}
    </label>
  )
}

// Small button that reads a file, for "fill from file" next to a text field.
export function FileButton({ accept, onFile, children }: { accept?: string; onFile: (f: File) => void; children: ReactNode }) {
  return (
    <label className="btn btn-sm" style={{ position: 'relative' }}>
      <input className="sr-only" type="file" accept={accept} onChange={(e) => { const f = e.target.files?.[0]; if (f) onFile(f); e.target.value = '' }} />
      <Icon name="upload" size={14} />{children}
    </label>
  )
}

export function Tabs<T extends string>({ tabs, value, onChange }: { tabs: [T, ReactNode][]; value: T; onChange: (v: T) => void }) {
  const listRef = useRef<HTMLDivElement>(null)
  const mountedRef = useRef(false)
  useEffect(() => {
    const active = listRef.current?.querySelector<HTMLElement>('[aria-selected="true"]')
    if (!active) {
      mountedRef.current = true
      return
    }
    const behavior: ScrollBehavior = mountedRef.current ? 'smooth' : 'auto'
    mountedRef.current = true
    active.scrollIntoView?.({ inline: 'nearest', block: 'nearest', behavior })
  }, [value, tabs.length])
  return (
    <div ref={listRef} className="tabs" role="tablist">
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

// ---------- combobox ----------
// Free-text input with a suggestion list: type anything, or open the list (chevron, ArrowDown)
// and pick. Typing filters the list; the menu shares Select's look and portal placement.

export type ComboOption = { value: string; label?: ReactNode; hint?: ReactNode; disabled?: boolean }

let comboSeq = 0

export function Combobox({ value, onChange, options, placeholder, className, mono, autoFocus, ...aria }: {
  value: string
  onChange: (v: string) => void
  options: ComboOption[]
  placeholder?: string
  className?: string
  mono?: boolean
  autoFocus?: boolean
  'aria-label'?: string
}) {
  const [open, setOpen] = useState(false)
  const [filter, setFilter] = useState(false) // filter by the typed text (off when opened from the chevron)
  const [active, setActive] = useState(-1)
  const [pos, setPos] = useState<MenuPlacement | null>(null)
  const boxRef = useRef<HTMLDivElement>(null)
  const inputRef = useRef<HTMLInputElement>(null)
  const listRef = useRef<HTMLUListElement>(null)
  const isMobile = useIsMobileSheet()
  const [uid] = useState(() => 'combo' + ++comboSeq)

  const q = value.trim().toLowerCase()
  const shown = filter && q
    ? options.filter((o) => o.value.toLowerCase().includes(q) || nodeText(o.label).toLowerCase().includes(q) || nodeText(o.hint).toLowerCase().includes(q))
    : options

  const place = useCallback(() => {
    const r = inputRef.current?.getBoundingClientRect() ?? boxRef.current?.getBoundingClientRect()
    if (!r) return
    setPos(calcMenuPlacement(r, getViewportBounds(), { maxPreferredWidth: 480, minPreferredWidth: 280 }))
  }, [])

  const show = (withFilter: boolean) => {
    place()
    setFilter(withFilter)
    setActive(-1)
    setOpen(true)
  }
  const close = useCallback(() => setOpen(false), [])
  const pick = (o: ComboOption | undefined) => {
    if (!o || o.disabled) return
    onChange(o.value)
    setOpen(false)
    inputRef.current?.focus()
  }
  const step = (from: number, dir: 1 | -1) => {
    for (let i = from + dir; i >= 0 && i < shown.length; i += dir) if (!shown[i].disabled) return i
    return from
  }

  useDropdownDismiss({
    open,
    floating: !isMobile,
    triggerRef: boxRef,
    listRef,
    onClose: close,
    onReposition: place,
    returnFocusOnEsc: false,
  })

  useEffect(() => {
    if (!open || !isMobile || shown.length === 0) return
    const id = requestAnimationFrame(() => listRef.current?.scrollIntoView({ block: 'nearest', behavior: 'smooth' }))
    return () => cancelAnimationFrame(id)
  }, [open, isMobile, shown.length])

  useEffect(() => {
    if (open && active >= 0) listRef.current?.children[active]?.scrollIntoView({ block: 'nearest' })
  }, [open, active])

  const onKey = (e: ReactKeyboardEvent<HTMLInputElement>) => {
    const k = e.key
    if (!open) {
      if (k === 'ArrowDown' || k === 'ArrowUp') { e.preventDefault(); show(false) }
      return
    }
    if (k === 'Escape') { e.preventDefault(); e.stopPropagation(); setOpen(false) }
    else if (k === 'Tab') setOpen(false)
    else if (k === 'ArrowDown') { e.preventDefault(); setActive((a) => step(a, 1)) }
    else if (k === 'ArrowUp') { e.preventDefault(); setActive((a) => step(a, -1)) }
    else if (k === 'Enter' && active >= 0) { e.preventDefault(); pick(shown[active]) }
  }

  const menuList = open && shown.length > 0 && (
    <ul
      ref={listRef}
      id={uid}
      role="listbox"
      className={'select-menu combo-menu' + (isMobile ? ' combo-inline' : '')}
      style={!isMobile && pos ? { left: pos.left, top: pos.top, bottom: pos.bottom, minWidth: pos.minWidth, maxWidth: pos.maxWidth, maxHeight: pos.maxHeight } : undefined}
      onClick={(e) => e.stopPropagation()}
    >
      {shown.map((o, i) => (
        <li key={o.value} id={`${uid}-${i}`} role="option" aria-selected={o.value === value} aria-disabled={o.disabled || undefined}
          className={(i === active ? 'active' : '') + (o.value === value ? ' selected' : '') + (o.disabled ? ' disabled' : '')}
          onMouseEnter={() => !o.disabled && setActive(i)}
          onMouseDown={(e) => e.preventDefault()}
          onClick={() => pick(o)}>
          <span className="combo-main">
            <span className="mono">{o.value}</span>
            {o.label && <span className="combo-label">{o.label}</span>}
          </span>
          {o.hint && <span className="combo-hint">{o.hint}</span>}
        </li>
      ))}
    </ul>
  )

  return (
    <div ref={boxRef} className={'combo' + (open ? ' open' : '') + (className ? ' ' + className : '')}>
      <div className="combo-box">
        <input
          ref={inputRef}
          className={'input' + (mono ? ' mono' : '')}
          value={value}
          placeholder={placeholder}
          autoFocus={autoFocus}
          autoComplete="off"
          spellCheck={false}
          role="combobox"
          aria-expanded={open}
          aria-controls={uid}
          aria-autocomplete="list"
          aria-activedescendant={open && active >= 0 ? `${uid}-${active}` : undefined}
          aria-label={aria['aria-label']}
          onChange={(e) => { onChange(e.target.value); if (!open) show(true); else { setFilter(true); setActive(-1) } }}
          onKeyDown={onKey}
        />
        <button type="button" className="combo-toggle" tabIndex={-1} aria-label="展开选项"
          onMouseDown={(e) => e.preventDefault()}
          onClick={(e) => { e.preventDefault(); if (open) setOpen(false); else { show(false); inputRef.current?.focus() } }}>
          <Icon name="chevron-down" size={16} />
        </button>
      </div>
      {isMobile ? menuList : (menuList && pos && createPortal(menuList, document.body))}
    </div>
  )
}

// ---------- select ----------
// Styled replacement for <select>. Takes the same <option> children, so call sites read the
// same; onChange receives the chosen value as a string. The menu is portalled to <body> so it
// is never clipped by modals or scrolling tables, and flips upward near the bottom edge.

type Opt = { value: string; label: ReactNode; text: string; disabled?: boolean }

function nodeText(n: ReactNode): string {
  if (n == null || typeof n === 'boolean') return ''
  if (typeof n === 'string' || typeof n === 'number') return String(n)
  if (Array.isArray(n)) return n.map(nodeText).join('')
  if (isValidElement(n)) return nodeText((n.props as { children?: ReactNode }).children)
  return ''
}

function readOptions(children: ReactNode): Opt[] {
  const out: Opt[] = []
  Children.forEach(children, (c) => {
    if (!isValidElement(c)) return
    if (c.type === Fragment) return void out.push(...readOptions((c.props as { children?: ReactNode }).children))
    const p = c.props as { value?: string | number; children?: ReactNode; disabled?: boolean }
    const text = nodeText(p.children)
    out.push({ value: p.value != null ? String(p.value) : text, label: p.children, text, disabled: p.disabled })
  })
  return out
}

let selectSeq = 0

export function Select({ value, onChange, children, disabled, style, className, placeholder, ...aria }: {
  value: string | number | undefined
  onChange: (v: string) => void
  children: ReactNode
  disabled?: boolean
  style?: CSSProperties
  className?: string
  placeholder?: string
  'aria-label'?: string
  id?: string
}) {
  const opts = readOptions(children)
  const cur = value == null ? '' : String(value)
  const selIdx = opts.findIndex((o) => o.value === cur)
  const sel = selIdx >= 0 ? opts[selIdx] : undefined
  const [open, setOpen] = useState(false)
  const [active, setActive] = useState(-1)
  const [pos, setPos] = useState<MenuPlacement | null>(null)
  const [sheetTitle, setSheetTitle] = useState(aria['aria-label'] || placeholder || '请选择')
  const isMobile = useIsMobileSheet()
  const btnRef = useRef<HTMLButtonElement>(null)
  const listRef = useRef<HTMLUListElement>(null)
  const typed = useRef({ s: '', t: 0 })
  const [uid] = useState(() => 'sel' + ++selectSeq)

  const place = useCallback(() => {
    const r = btnRef.current?.getBoundingClientRect()
    if (!r) return
    setPos(calcMenuPlacement(r, getViewportBounds(), {
      preferHeight: Math.min(320, opts.length * 34 + 10),
    }))
  }, [opts.length])

  const close = useCallback(() => setOpen(false), [])
  const show = () => {
    if (disabled) return
    place()
    const fieldLabel = btnRef.current?.closest('.field')?.querySelector(':scope > span')?.textContent?.trim()
    setSheetTitle(aria['aria-label'] || fieldLabel || placeholder || '请选择')
    setActive(selIdx >= 0 ? selIdx : opts.findIndex((o) => !o.disabled))
    setOpen(true)
  }
  const pick = (i: number) => {
    const o = opts[i]
    if (!o || o.disabled) return
    setOpen(false)
    btnRef.current?.focus()
    if (o.value !== cur) onChange(o.value)
  }
  const step = (from: number, dir: 1 | -1) => {
    for (let i = from + dir; i >= 0 && i < opts.length; i += dir) if (!opts[i].disabled) return i
    return from
  }

  useDropdownDismiss({
    open,
    floating: !isMobile,
    triggerRef: btnRef,
    listRef,
    onClose: close,
    onReposition: place,
  })

  useEffect(() => {
    if (open && isMobile) return lockBodyScroll()
  }, [open, isMobile])

  useEffect(() => {
    if (open && active >= 0) listRef.current?.children[active]?.scrollIntoView({ block: 'nearest' })
  }, [open, active])

  const onKey = (e: ReactKeyboardEvent) => {
    if (disabled) return
    const k = e.key
    if (!open) {
      if (k === 'ArrowDown' || k === 'ArrowUp' || k === 'Enter' || k === ' ') { e.preventDefault(); show() }
      return
    }
    if (k === 'Escape' || k === 'Tab') {
      if (k === 'Escape') { e.preventDefault(); e.stopPropagation() }
      setOpen(false)
    } else if (k === 'ArrowDown') { e.preventDefault(); setActive((a) => step(a, 1)) }
    else if (k === 'ArrowUp') { e.preventDefault(); setActive((a) => step(a, -1)) }
    else if (k === 'Home') { e.preventDefault(); setActive(step(-1, 1)) }
    else if (k === 'End') { e.preventDefault(); setActive(step(opts.length, -1)) }
    else if (k === 'Enter' || k === ' ') { e.preventDefault(); pick(active) }
    else if (k.length === 1) {
      const now = Date.now()
      typed.current = { s: (now - typed.current.t < 600 ? typed.current.s : '') + k.toLowerCase(), t: now }
      const i = opts.findIndex((o) => !o.disabled && o.text.toLowerCase().startsWith(typed.current.s))
      if (i >= 0) setActive(i)
    }
  }

  const optionNodes = (
    <>
      {opts.map((o, i) => (
        <li
          key={o.value + ':' + i}
          id={`${uid}-${i}`}
          role="option"
          aria-selected={i === selIdx}
          aria-disabled={o.disabled || undefined}
          className={(i === active ? 'active' : '') + (i === selIdx ? ' selected' : '') + (o.disabled ? ' disabled' : '')}
          onMouseEnter={() => !o.disabled && setActive(i)}
          onClick={() => pick(i)}
        >
          <span className="grow">{o.label}</span>
          {i === selIdx && <Icon name="check" size={16} />}
        </li>
      ))}
      {opts.length === 0 && <li className="disabled">无可选项</li>}
    </>
  )

  return (
    <>
      <button
        ref={btnRef}
        type="button"
        id={aria.id}
        className={'select' + (open ? ' open' : '') + (className ? ' ' + className : '')}
        style={style}
        disabled={disabled}
        role="combobox"
        aria-label={aria['aria-label']}
        aria-haspopup="listbox"
        aria-expanded={open}
        aria-controls={open ? uid : undefined}
        aria-activedescendant={open && active >= 0 ? `${uid}-${active}` : undefined}
        onClick={() => (open ? setOpen(false) : show())}
        onKeyDown={onKey}
      >
        <span className={'select-value' + (sel ? '' : ' placeholder')}>{sel ? sel.label : placeholder ?? '请选择'}</span>
        <Icon name="chevron-down" size={16} />
      </button>
      {open && isMobile && createPortal(
        <div
          className="select-sheet-backdrop"
          onMouseDown={(e) => { if (e.target === e.currentTarget) { e.stopPropagation(); setOpen(false) } }}
          onClick={(e) => e.stopPropagation()}
        >
          <div className="select-sheet" role="dialog" aria-modal="true" aria-label={sheetTitle}>
            <div className="select-sheet-grip" aria-hidden="true" />
            <div className="select-sheet-head">
              <span className="select-sheet-title">{sheetTitle}</span>
              <button
                type="button"
                className="btn btn-sm btn-ghost btn-icon"
                aria-label="关闭"
                onClick={() => { setOpen(false); btnRef.current?.focus() }}
              >
                <Icon name="x" size={16} />
              </button>
            </div>
            <ul ref={listRef} id={uid} role="listbox" className="select-sheet-list">
              {optionNodes}
            </ul>
          </div>
        </div>,
        document.body,
      )}
      {open && !isMobile && pos && createPortal(
        <ul
          ref={listRef}
          id={uid}
          role="listbox"
          className="select-menu"
          style={{ left: pos.left, top: pos.top, bottom: pos.bottom, minWidth: pos.minWidth, maxWidth: pos.maxWidth, maxHeight: pos.maxHeight }}
          onMouseDown={(e) => e.preventDefault()}
        >
          {optionNodes}
        </ul>,
        document.body,
      )}
    </>
  )
}

// ---------- icons ----------
// One icon set for the whole panel: Lucide, fixed 1.5px stroke at every size. Use <Icon name="..."> everywhere
// so nothing ships a one-off SVG with a different weight or style.

const icons: Record<string, LucideIcon> = {
  overview: LayoutDashboard,
  projects: Layers,
  image: Package,
  volume: HardDrive,
  build: Hammer,
  docker: Container,
  databases: Database,
  backups: HardDriveDownload,
  sites: Globe,
  certs: ShieldCheck,
  jobs: ListChecks,
  integrations: Plug,
  access: Users,
  settings: Settings,
  logout: LogOut,
  x: X,
  plus: Plus,
  menu: Menu,
  play: Play,
  stop: Square,
  restart: RotateCcw,
  rocket: Rocket,
  template: LayoutTemplate,
  terminal: SquareTerminal,
  logs: ScrollText,
  trash: Trash2,
  download: Download,
  refresh: RefreshCw,
  check: Check,
  lock: Lock,
  key: KeyRound,
  copy: Copy,
  edit: Pencil,
  search: Search,
  alert: TriangleAlert,
  git: GitBranch,
  folder: Folder,
  file: File,
  upload: Upload,
  chart: ChartLine,
  clock: Clock,
  link: Link,
  external: ExternalLink,
  'chevron-down': ChevronDown,
  'chevron-left': ChevronLeft,
  'chevron-right': ChevronRight,
}

export function Icon({ name, size = 18 }: { name: string; size?: number }) {
  const C = icons[name]
  if (!C) return null
  return <C size={size} strokeWidth={1.5} absoluteStrokeWidth aria-hidden="true" style={{ flex: 'none' }} />
}
