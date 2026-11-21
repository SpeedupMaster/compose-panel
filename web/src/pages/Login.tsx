import { FormEvent, useEffect, useState } from 'react'
import { Navigate, useNavigate, useSearchParams } from 'react-router-dom'
import { get, post, uploadForm } from '../api'
import { useAuth } from '../auth'
import { Logo } from '../components/Layout'
import { PanelRestoreSummary, PanelSummaryGrid, RestartWaitOverlay } from '../components/PanelRestoreModal'
import { ErrorBox, Field, FilePicker, Icon } from '../components/ui'

export default function Login() {
  const { me, refresh } = useAuth()
  const [params] = useSearchParams()
  const nav = useNavigate()
  const next = params.get('next') || '/'
  const restoredNotice = params.get('restored') === '1'
  const [mode, setMode] = useState<'loading' | 'login' | 'totp' | 'setup'>('loading')
  const [setupTab, setSetupTab] = useState<'create' | 'restore'>('create')
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [password2, setPassword2] = useState('')
  const [email, setEmail] = useState('')
  const [token, setToken] = useState('')
  const [code, setCode] = useState('')
  const [file, setFile] = useState<File | null>(null)
  const [ageIdentity, setAgeIdentity] = useState('')
  const [masterKey, setMasterKey] = useState('')
  const [summary, setSummary] = useState<PanelRestoreSummary | null>(null)
  const [progress, setProgress] = useState<number | null>(null)
  const [restarting, setRestarting] = useState(false)
  const [prevBootId, setPrevBootId] = useState<string | undefined>()
  const [err, setErr] = useState<unknown>()
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    get('/setup/status')
      .then((s) => setMode(s.needed ? 'setup' : 'login'))
      .catch(() => setMode('login'))
  }, [])

  if (me) return <Navigate to={next} replace />

  const runSetupRestore = async (dryRun: boolean) => {
    if (!token.trim()) throw new Error('请填写初始化令牌')
    if (!file) throw new Error('请选择面板备份文件')
    const fd = new FormData()
    fd.append('token', token.trim())
    fd.append('file', file)
    if (ageIdentity.trim()) fd.append('ageIdentity', ageIdentity.trim())
    if (masterKey.trim()) fd.append('masterKey', masterKey.trim())
    if (dryRun) fd.append('dryRun', '1')
    setProgress(0)
    try {
      const r = await uploadForm('/setup/restore', fd, setProgress, { 'X-Setup-Token': token.trim() })
      if (dryRun) {
        setSummary(r.summary)
      } else if (r?.restarting) {
        setPrevBootId(r.bootId)
        setRestarting(true)
      }
    } finally {
      setProgress(null)
    }
  }

  const submit = async (e: FormEvent) => {
    e.preventDefault()
    setErr(undefined)
    setBusy(true)
    try {
      if (mode === 'setup') {
        if (setupTab === 'restore') {
          await runSetupRestore(!summary?.keyMatched)
          return
        }
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

  const resetRestoreCheck = () => {
    setSummary(null)
    setErr(undefined)
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
            <div className="seg" role="group" aria-label="初始化方式">
              <button type="button" className={setupTab === 'create' ? 'on' : ''} onClick={() => { setSetupTab('create'); setErr(undefined) }}>创建管理员</button>
              <button type="button" className={setupTab === 'restore' ? 'on' : ''} onClick={() => { setSetupTab('restore'); setErr(undefined) }}>从备份恢复面板</button>
            </div>
            {setupTab === 'create' ? (
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
            ) : (
              <>
                <div>
                  <h1 className="h1" style={{ fontSize: 20 }}>从备份恢复面板</h1>
                  <p className="muted" style={{ margin: '4px 0 0' }}>直接恢复原服务器的 <code>panel.db</code> 与 <code>master.key</code>，无需先建临时账号。</p>
                </div>
                <Field label="初始化令牌" hint="见 docker logs panel 中的 setup token">
                  <input className="input mono" value={token} onChange={(e) => setToken(e.target.value)} required autoFocus />
                </Field>
                <Field label="面板备份文件" div>
                  <FilePicker
                    file={file}
                    onChange={(f) => { setFile(f); resetRestoreCheck() }}
                    accept=".db,.sqlite,.sqlite3,.zst,.age,.gz"
                    disabled={busy}
                    hint="支持 .db / .sqlite / .sqlite.zst / .sqlite.zst.age"
                  />
                </Field>
                <Field label="age 解密私钥（若备份已加密）" hint="以 AGE-SECRET-KEY-1 开头；仅本次恢复使用">
                  <input className="input mono" type="password" value={ageIdentity} onChange={(e) => { setAgeIdentity(e.target.value); resetRestoreCheck() }} placeholder="未加密可留空" autoComplete="off" />
                </Field>
                <Field label="原服务器 master.key" hint="64 位十六进制主密钥；用于解密原面板中的所有凭据">
                  <input className="input mono" type="password" value={masterKey} onChange={(e) => { setMasterKey(e.target.value); resetRestoreCheck() }} placeholder="64 位十六进制" autoComplete="off" />
                </Field>
                {progress != null && (
                  <div className="col" style={{ gap: 4 }}>
                    <div className="progress"><div style={{ width: progress + '%' }} /></div>
                    <span className="label">上传中 {progress}%</span>
                  </div>
                )}
                {summary && <PanelSummaryGrid summary={summary} />}
              </>
            )}
          </>
        )}
        {mode === 'login' && (
          <>
            <h1 className="h1" style={{ fontSize: 20 }}>登录</h1>
            {restoredNotice && <div className="alert ok"><Icon name="check" />面板数据已恢复并重启，请使用备份中的原管理员账号重新登录。</div>}
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
          mode === 'setup' && setupTab === 'restore' ? (
            <div className="row wrap" style={{ gap: 8 }}>
              {summary?.keyMatched && (
                <button type="button" className="btn" style={{ height: 40 }} disabled={busy} onClick={() => { setBusy(true); setErr(undefined); runSetupRestore(true).catch(setErr).finally(() => setBusy(false)) }}>
                  重新检查
                </button>
              )}
              <button
                className={'btn grow ' + (summary?.keyMatched ? 'btn-danger solid' : 'btn-primary')}
                style={{ height: 40 }}
                disabled={busy || !token.trim() || !file}
              >
                {busy ? '处理中…' : summary?.keyMatched ? '确认恢复并重启面板' : '检查备份'}
              </button>
            </div>
          ) : (
            <button className="btn btn-primary" style={{ height: 40 }} disabled={busy}>
              {busy ? '请稍候…' : mode === 'setup' ? '创建并登录' : mode === 'totp' ? '验证' : '登录'}
            </button>
          )
        )}
        {mode === 'login' && <p className="label" style={{ margin: 0 }}>忘记密码？在服务器上运行 <code className="mono">panel reset-password &lt;用户名&gt;</code>。</p>}
      </form>
      {restarting && <RestartWaitOverlay title="正在从备份恢复面板…" prevBootId={prevBootId} />}
    </div>
  )
}
