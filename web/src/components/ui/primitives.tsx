import { ReactNode, useEffect, useRef, useState } from 'react'
import { Icon } from './icons'
import { Select } from './select'

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
