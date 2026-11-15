import { FormEvent, useEffect, useState } from 'react'
import { Navigate, useNavigate, useSearchParams } from 'react-router-dom'
import { get, post } from '../api'
import { useAuth } from '../auth'
import { Logo } from '../components/Layout'
import { ErrorBox, Field } from '../components/ui'

export default function Login() {
  const { me, refresh } = useAuth()
  const [params] = useSearchParams()
  const nav = useNavigate()
  const next = params.get('next') || '/'
  const [mode, setMode] = useState<'loading' | 'login' | 'totp' | 'setup'>('loading')
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [password2, setPassword2] = useState('')
  const [email, setEmail] = useState('')
  const [token, setToken] = useState('')
  const [code, setCode] = useState('')
  const [err, setErr] = useState<unknown>()
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    get('/setup/status')
      .then((s) => setMode(s.needed ? 'setup' : 'login'))
      .catch(() => setMode('login'))
  }, [])

  if (me) return <Navigate to={next} replace />

  const submit = async (e: FormEvent) => {
    e.preventDefault()
    setErr(undefined)
    setBusy(true)
    try {
      if (mode === 'setup') {
        if (password !== password2) throw new Error('两次输入的密码不一致')
        await post('/setup', { username, password, email, token })
        await refresh()
      } else if (mode === 'login') {
        const r = await post('/auth/login', { username, password })
        if (r.mfa) {
          setMode('totp')
          return
        }
        await refresh()
      } else {
        await post('/auth/totp', { code: code.trim() })
        await refresh()
      }
      nav(next, { replace: true })
    } catch (e) {
      setErr(e)
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="auth-page">
      <form className="auth-card" onSubmit={submit}>
        <div className="row" style={{ gap: 12 }}>
          <Logo size={40} />
          <div>
            <div className="st">Compose 面板</div>
            <div className="label">轻量的 Docker Compose 管理</div>
          </div>
        </div>
        {mode === 'loading' && <div className="muted">加载中…</div>}
        {mode === 'setup' && (
          <>
            <div>
              <h1 className="h1" style={{ fontSize: 20 }}>创建管理员</h1>
              <p className="muted" style={{ margin: '4px 0 0' }}>首次启动，请创建第一个管理员账号。初始化令牌在面板启动日志中。</p>
            </div>
            <Field label="初始化令牌" hint="见 docker logs panel 中的 setup token">
              <input className="input mono" value={token} onChange={(e) => setToken(e.target.value)} required autoFocus />
            </Field>
            <Field label="用户名">
              <input className="input" value={username} onChange={(e) => setUsername(e.target.value)} required autoComplete="username" />
            </Field>
            <Field label="邮箱（可选）">
              <input className="input" type="email" value={email} onChange={(e) => setEmail(e.target.value)} />
            </Field>
            <Field label="密码" hint="至少 10 位">
              <input className="input" type="password" value={password} onChange={(e) => setPassword(e.target.value)} required minLength={10} autoComplete="new-password" />
            </Field>
            <Field label="确认密码">
              <input className="input" type="password" value={password2} onChange={(e) => setPassword2(e.target.value)} required autoComplete="new-password" />
            </Field>
          </>
        )}
        {mode === 'login' && (
          <>
            <h1 className="h1" style={{ fontSize: 20 }}>登录</h1>
            <Field label="用户名">
              <input className="input" value={username} onChange={(e) => setUsername(e.target.value)} required autoFocus autoComplete="username" />
            </Field>
            <Field label="密码">
              <input className="input" type="password" value={password} onChange={(e) => setPassword(e.target.value)} required autoComplete="current-password" />
            </Field>
          </>
        )}
        {mode === 'totp' && (
          <>
            <div>
              <h1 className="h1" style={{ fontSize: 20 }}>两步验证</h1>
              <p className="muted" style={{ margin: '4px 0 0' }}>输入验证器中的 6 位动态码，或一个恢复码。</p>
            </div>
            <Field label="验证码">
              <input className="input mono" value={code} onChange={(e) => setCode(e.target.value)} required autoFocus inputMode="numeric" autoComplete="one-time-code" />
            </Field>
          </>
        )}
        <ErrorBox error={err} />
        {mode !== 'loading' && (
          <button className="btn btn-primary" style={{ height: 40 }} disabled={busy}>
            {busy ? '请稍候…' : mode === 'setup' ? '创建并登录' : mode === 'totp' ? '验证' : '登录'}
          </button>
        )}
        {mode === 'login' && <p className="label" style={{ margin: 0 }}>忘记密码？在服务器上运行 <code className="mono">panel reset-password &lt;用户名&gt;</code>。</p>}
      </form>
    </div>
  )
}
