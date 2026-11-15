import { useState } from 'react'
import { Link, useNavigate } from 'react-router-dom'
import { get, post } from '../api'
import { CodeArea, EnvEditor, EnvVar, validateCompose, Validation, ValidationResult } from '../components/editors'
import { Card, ErrorBox, Field, Page, useJobLog, useLoad, useToast } from '../components/ui'

type Template = { id: string; name: string; description: string; compose: string; env: EnvVar[] | null; service: string; port: number }

export default function ProjectNew() {
  const nav = useNavigate()
  const toast = useToast()
  const openJob = useJobLog()
  const { data: templates } = useLoad(() => get<Template[]>('/templates'))
  const [source, setSource] = useState<'inline' | 'git'>('inline')
  const [name, setName] = useState('')
  const [description, setDescription] = useState('')
  const [compose, setCompose] = useState('services:\n  web:\n    image: \n    restart: unless-stopped\n')
  const [env, setEnv] = useState<EnvVar[]>([])
  const [gitUrl, setGitUrl] = useState('')
  const [gitBranch, setGitBranch] = useState('main')
  const [gitComposePath, setGitComposePath] = useState('compose.yml')
  const [autoDeploy, setAutoDeploy] = useState(true)
  const [domain, setDomain] = useState('')
  const [service, setService] = useState('')
  const [port, setPort] = useState('')
  const [deploy, setDeploy] = useState(true)
  const [v, setV] = useState<Validation | null>(null)
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState<unknown>()

  const applyTemplate = (t: Template) => {
    setSource('inline')
    setCompose(t.compose)
    setEnv(t.env || [])
    if (!name) setName(t.id)
    setService(t.service || '')
    setPort(t.port ? String(t.port) : '')
    setV(null)
  }

  const validate = async () => {
    try {
      const r = await validateCompose(compose, env, name)
      setV(r)
      if (r.valid && r.services?.length && !service) setService(r.services[0])
      return r
    } catch (e) {
      toast.err(e)
    }
  }

  const submit = async () => {
    setErr(undefined)
    if (source === 'inline') {
      const r = await validate()
      if (!r?.valid) return
    }
    setBusy(true)
    try {
      const res = await post('/projects', { name, description, source, compose: source === 'inline' ? compose : '', env, gitUrl, gitBranch, gitComposePath, autoDeploy, deploy, options: {} })
      if (domain.trim()) {
        const svc = service || v?.services?.[0] || ''
        try {
          const r = await post('/sites', { domains: domain.split(/[\s,]+/).filter(Boolean), kind: 'proxy', projectId: res.id, service: svc, port: Number(port) || 80, hsts: true, compress: true, enabled: true })
          if (r.warning) toast.err(r.warning)
        } catch (e) {
          toast.err('项目已创建，但站点创建失败：' + (e as Error).message)
        }
      }
      toast.ok('项目已创建')
      if (res.jobId) openJob(res.jobId, `部署 ${name}`)
      nav(`/projects/${res.id}`)
    } catch (e) {
      setErr(e)
    } finally {
      setBusy(false)
    }
  }

  return (
    <Page title="新建项目" crumbs={<><Link to="/projects">项目</Link> / 新建</>}>
      <Card title="基本信息">
        <div className="form-grid">
          <Field label="项目名" hint="小写字母、数字、- 和 _；也是 Compose 项目名">
            <input className="input mono" value={name} onChange={(e) => setName(e.target.value.toLowerCase())} placeholder="my-app" autoFocus />
          </Field>
          <Field label="描述（可选）">
            <input className="input" value={description} onChange={(e) => setDescription(e.target.value)} />
          </Field>
        </div>
        <div className="row" style={{ marginTop: 16 }}>
          <span className="label">来源</span>
          <div className="seg" role="group" aria-label="来源">
            <button className={source === 'inline' ? 'on' : ''} onClick={() => setSource('inline')}>在面板中编写</button>
            <button className={source === 'git' ? 'on' : ''} onClick={() => setSource('git')}>Git 仓库</button>
          </div>
        </div>
      </Card>

      {source === 'inline' ? (
        <>
          {templates && templates.length > 0 && (
            <Card title="从模板开始（可选）">
              <div className="grid">
                {templates.map((t) => (
                  <button key={t.id} className="card" style={{ textAlign: 'left', padding: 14, cursor: 'pointer' }} onClick={() => applyTemplate(t)}>
                    <div style={{ fontWeight: 600 }}>{t.name}</div>
                    <div className="label">{t.description}</div>
                  </button>
                ))}
              </div>
            </Card>
          )}
          <Card title="compose.yml" actions={<button className="btn btn-sm" onClick={validate}>校验</button>}>
            <div className="col" style={{ gap: 12 }}>
              <CodeArea label="compose.yml" value={compose} onChange={(x) => { setCompose(x); setV(null) }} />
              <ValidationResult v={v} />
              <p className="label" style={{ margin: 0 }}>面板会自动把服务接入代理网络并生成覆盖文件，无需在这里写 networks 或端口映射。</p>
            </div>
          </Card>
        </>
      ) : (
        <Card title="Git 仓库">
          <div className="form-grid">
            <Field label="仓库地址" hint="HTTPS 或 SSH（git@…）。SSH 仓库创建后会生成部署公钥。">
              <input className="input mono" value={gitUrl} onChange={(e) => setGitUrl(e.target.value)} placeholder="git@github.com:me/app.git" />
            </Field>
            <Field label="分支">
              <input className="input mono" value={gitBranch} onChange={(e) => setGitBranch(e.target.value)} />
            </Field>
            <Field label="Compose 文件路径">
              <input className="input mono" value={gitComposePath} onChange={(e) => setGitComposePath(e.target.value)} />
            </Field>
          </div>
          <label className="check" style={{ marginTop: 12 }}>
            <input type="checkbox" checked={autoDeploy} onChange={(e) => setAutoDeploy(e.target.checked)} />收到 Webhook 推送时自动部署
          </label>
          {(gitUrl.startsWith('git@') || gitUrl.startsWith('ssh://')) && (
            <div className="alert warn" style={{ marginTop: 12 }}>SSH 仓库需要先把部署公钥添加到仓库，再进行首次部署。建议先不勾选「创建后立即部署」。</div>
          )}
        </Card>
      )}

      <Card title="环境变量">
        <EnvEditor env={env} onChange={setEnv} />
      </Card>

      <Card title="域名（可选）">
        <div className="form-grid">
          <Field label="域名" hint="多个用逗号分隔；会自动申请 HTTPS 证书">
            <input className="input mono" value={domain} onChange={(e) => setDomain(e.target.value)} placeholder="app.example.com" />
          </Field>
          <Field label="服务">
            {v?.services?.length ? (
              <select className="select" value={service} onChange={(e) => setService(e.target.value)}>
                {v.services.map((s) => <option key={s}>{s}</option>)}
              </select>
            ) : (
              <input className="input mono" value={service} onChange={(e) => setService(e.target.value)} placeholder="web" />
            )}
          </Field>
          <Field label="容器端口">
            <input className="input mono" value={port} onChange={(e) => setPort(e.target.value)} placeholder="80" inputMode="numeric" />
          </Field>
        </div>
      </Card>

      <ErrorBox error={err} />
      <div className="row wrap">
        <label className="check"><input type="checkbox" checked={deploy} onChange={(e) => setDeploy(e.target.checked)} />创建后立即部署</label>
        <span className="spacer" />
        <Link className="btn" to="/projects">取消</Link>
        <button className="btn btn-primary" disabled={busy || !name} onClick={submit}>{busy ? '创建中…' : '创建项目'}</button>
      </div>
    </Page>
  )
}
