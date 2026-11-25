import { createContext, ReactNode, useCallback, useContext, useEffect, useState } from 'react'
import { ApiError, get, onUnauthorized } from './api'

export type Me = {
  user: { id: number; username: string; email: string; role: 'viewer' | 'operator' | 'admin'; totp: boolean; projects: number[] | null }
  version: string
  panelDomain: string
}

type Ctx = { me: Me | null; refresh: () => Promise<void>; setMe: (m: Me | null) => void }
const AuthCtx = createContext<Ctx>({ me: null, refresh: async () => {}, setMe: () => {} })

export function AuthProvider({ children }: { children: ReactNode }) {
  const [me, setMe] = useState<Me | null>(null)
  const [ready, setReady] = useState(false)
  // The panel could not be reached (offline, or the proxy answered 5xx). Distinct from being
  // logged out: an installed app opened without a connection should say so, not ask for a login.
  const [unreachable, setUnreachable] = useState(false)
  const refresh = useCallback(async () => {
    try {
      setMe(await get<Me>('/auth/me'))
      setUnreachable(false)
    } catch (e) {
      if (e instanceof ApiError && e.status > 0 && e.status < 500) {
        setMe(null)
        setUnreachable(false)
      } else {
        setUnreachable(true)
      }
    } finally {
      setReady(true)
    }
  }, [])
  useEffect(() => {
    refresh()
  }, [refresh])
  useEffect(() => {
    if (!unreachable) return
    const retry = () => refresh()
    window.addEventListener('online', retry)
    const t = setInterval(() => navigator.onLine && retry(), 15000)
    return () => {
      window.removeEventListener('online', retry)
      clearInterval(t)
    }
  }, [unreachable, refresh])
  useEffect(() => {
    const off = onUnauthorized(() => setMe(null))
    return () => {
      off()
    }
  }, [])
  if (!ready) return null
  if (unreachable && !me) return <Unreachable onRetry={refresh} />
  return <AuthCtx.Provider value={{ me, refresh, setMe }}>{children}</AuthCtx.Provider>
}

function Unreachable({ onRetry }: { onRetry: () => Promise<void> }) {
  const [busy, setBusy] = useState(false)
  const retry = async () => {
    setBusy(true)
    await onRetry()
    setBusy(false)
  }
  return (
    <div className="auth-page">
      <div className="auth-card">
        <div className="row" style={{ gap: 12 }}>
          <svg width={40} height={40} viewBox="0 0 32 32" aria-hidden="true">
            <rect width="32" height="32" rx="8" fill="#B54708" />
            <path d="M9 11h14M9 16h14M9 21h9" stroke="#fff" strokeWidth="2.4" strokeLinecap="round" />
          </svg>
          <div>
            <div className="st">Compose 面板</div>
            <div className="label">轻量的 Docker Compose 管理</div>
          </div>
        </div>
        <div>
          <h1 className="h1" style={{ fontSize: 20 }}>无法连接到面板</h1>
          <p className="muted" style={{ margin: '4px 0 0' }}>
            {navigator.onLine ? '服务器暂时没有响应，可能正在重启或升级。' : '设备当前处于离线状态。'}恢复连接后会自动重试。
          </p>
        </div>
        <button className="btn btn-primary" disabled={busy} onClick={retry}>
          {busy ? '正在重试…' : '重试'}
        </button>
      </div>
    </div>
  )
}

export const useAuth = () => useContext(AuthCtx)

const levels = { viewer: 1, operator: 2, admin: 3 }
export function useCan() {
  const { me } = useAuth()
  const lvl = me ? levels[me.user.role] : 0
  return { operator: lvl >= 2, admin: lvl >= 3 }
}

export const roleName: Record<string, string> = { viewer: '只读', operator: '运维', admin: '管理员' }
