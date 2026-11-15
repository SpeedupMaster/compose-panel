import { useEffect, useState } from 'react'
import { NavLink, Outlet, useLocation, useNavigate } from 'react-router-dom'
import { get, post } from '../api'
import { roleName, useAuth, useCan } from '../auth'
import { Icon, useJobLog } from './ui'

type NavItem = { to: string; label: string; icon: string; admin?: boolean; badge?: number }

export default function Layout() {
  const { me, setMe } = useAuth()
  const can = useCan()
  const nav = useNavigate()
  const loc = useLocation()
  const [open, setOpen] = useState(false)
  const [active, setActive] = useState<any[]>([])
  const [alerts, setAlerts] = useState({ unhealthy: 0, certs: 0, backups: 0 })
  const openJob = useJobLog()

  useEffect(() => setOpen(false), [loc.pathname])
  useEffect(() => {
    let stop = false
    const tick = async () => {
      if (document.hidden) return
      try {
        const jobs = await get<any[]>('/jobs?status=active')
        if (!stop) setActive(jobs)
      } catch {}
    }
    const tickAlerts = async () => {
      if (document.hidden) return
      try {
        const o = await get<any>('/overview')
        if (!stop) setAlerts({ unhealthy: o.unhealthy?.length || 0, certs: (o.certs || []).filter((c: any) => c.daysLeft <= 14).length, backups: o.backups?.failed || 0 })
      } catch {}
    }
    tick()
    tickAlerts()
    const a = setInterval(tick, 4000)
    const b = setInterval(tickAlerts, 60000)
    return () => {
      stop = true
      clearInterval(a)
      clearInterval(b)
    }
  }, [])

  const groups: { title?: string; items: NavItem[] }[] = [
    { items: [{ to: '/', label: '总览', icon: 'overview' }, { to: '/projects', label: '项目', icon: 'projects', badge: alerts.unhealthy }] },
    {
      title: '运行时',
      items: [
        { to: '/docker', label: 'Docker 资源', icon: 'docker' },
        { to: '/databases', label: '数据库', icon: 'databases' },
        { to: '/backups', label: '备份', icon: 'backups', badge: alerts.backups },
      ],
    },
    { title: '网络', items: [{ to: '/sites', label: '站点', icon: 'sites' }, { to: '/certificates', label: '证书', icon: 'certs', badge: alerts.certs }] },
    {
      title: '系统',
      items: [
        { to: '/jobs', label: '任务中心', icon: 'jobs' },
        { to: '/integrations', label: '存储与通知', icon: 'integrations', admin: true },
        { to: '/access', label: '用户与审计', icon: 'access' },
        { to: '/settings', label: '设置', icon: 'settings' },
      ],
    },
  ]

  const logout = async () => {
    await post('/auth/logout').catch(() => {})
    setMe(null)
    nav('/login')
  }

  const u = me!.user
  return (
    <div className="app">
      <div className="mobile-bar">
        <button aria-label="打开菜单" onClick={() => setOpen(true)}><Icon name="menu" /></button>
        <b>Compose 面板</b>
      </div>
      <aside className={'side' + (open ? ' open' : '')} aria-label="主导航">
        <div className="brand">
          <Logo />
          <div>
            <b>Compose 面板</b>
            <small>v{me!.version}</small>
          </div>
        </div>
        {active.length > 0 ? (
          <button className="jobs" style={{ border: 0, cursor: 'pointer', textAlign: 'left' }} onClick={() => openJob(active[0].id, active[0].title)}>
            <span className="grow ellipsis">运行中任务 · {active[0].title}</span>
            <span className="pill info">{active.length}</span>
          </button>
        ) : (
          <NavLink to="/jobs" className="jobs">
            <span className="grow">没有运行中的任务</span>
          </NavLink>
        )}
        {groups.map((g, i) => (
          <nav className="group" key={i} aria-label={g.title}>
            {g.title && <div className="group-title">{g.title}</div>}
            {g.items
              .filter((it) => !it.admin || can.admin)
              .map((it) => (
                <NavLink key={it.to} to={it.to} end={it.to === '/'} className={({ isActive }) => 'nav' + (isActive ? ' active' : '')}>
                  <Icon name={it.icon} />
                  <span className="grow">{it.label}</span>
                  {!!it.badge && <span className="pill alert">{it.badge}</span>}
                </NavLink>
              ))}
          </nav>
        ))}
        <div className="me">
          <div className="avatar" aria-hidden="true">{u.username.slice(0, 1).toUpperCase()}</div>
          <div className="grow" style={{ minWidth: 0 }}>
            <div className="ellipsis" style={{ fontWeight: 600 }}>{u.username}</div>
            <div style={{ fontSize: 12, color: '#A3A5AA' }}>{roleName[u.role]} · v{me!.version}</div>
          </div>
          <button className="icon-btn" aria-label="退出登录" title="退出登录" onClick={logout}><Icon name="logout" /></button>
        </div>
      </aside>
      <main className="main">
        <Outlet />
      </main>
    </div>
  )
}

export function Logo({ size = 32 }: { size?: number }) {
  return (
    <svg width={size} height={size} viewBox="0 0 32 32" aria-hidden="true">
      <rect width="32" height="32" rx="8" fill="#B54708" />
      <path d="M9 11h14M9 16h14M9 21h9" stroke="#fff" strokeWidth="2.4" strokeLinecap="round" />
    </svg>
  )
}
