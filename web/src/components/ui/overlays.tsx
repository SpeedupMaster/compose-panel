import { createContext, ReactNode, useCallback, useContext, useEffect, useState } from 'react'
import { ApiError } from '../../api'
import { useAppUpdated } from '../../pwa'
import { lockBodyScroll, useEscapeLayer } from './hooks'
import { Icon } from './icons'
import { ErrorBox } from './primitives'

// ---------- modal ----------

export function Modal({
  title,
  onClose,
  children,
  footer,
  wide,
  closable = true,
}: {
  title: ReactNode
  onClose: () => void
  children: ReactNode
  footer?: ReactNode
  wide?: boolean
  closable?: boolean
}) {
  useEffect(() => lockBodyScroll(), [])
  useEscapeLayer(true, () => {
    if (closable) onClose()
  })
  return (
    <div className="overlay" onMouseDown={(e) => closable && e.target === e.currentTarget && onClose()}>
      <div className={'modal' + (wide ? ' wide' : '')} role="dialog" aria-modal="true">
        <div className="modal-head">
          <h2 className="st">{title}</h2>
          {closable && (
            <button className="btn btn-ghost btn-sm btn-icon" aria-label="关闭" onClick={onClose}>
              <Icon name="x" />
            </button>
          )}
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
