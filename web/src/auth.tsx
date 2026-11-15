import { createContext, ReactNode, useCallback, useContext, useEffect, useState } from 'react'
import { get, onUnauthorized } from './api'

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
  const refresh = useCallback(async () => {
    try {
      setMe(await get<Me>('/auth/me'))
    } catch {
      setMe(null)
    } finally {
      setReady(true)
    }
  }, [])
  useEffect(() => {
    refresh()
  }, [refresh])
  useEffect(() => {
    const off = onUnauthorized(() => setMe(null))
    return () => {
      off()
    }
  }, [])
  if (!ready) return null
  return <AuthCtx.Provider value={{ me, refresh, setMe }}>{children}</AuthCtx.Provider>
}

export const useAuth = () => useContext(AuthCtx)

const levels = { viewer: 1, operator: 2, admin: 3 }
export function useCan() {
  const { me } = useAuth()
  const lvl = me ? levels[me.user.role] : 0
  return { operator: lvl >= 2, admin: lvl >= 3 }
}

export const roleName: Record<string, string> = { viewer: '只读', operator: '运维', admin: '管理员' }
