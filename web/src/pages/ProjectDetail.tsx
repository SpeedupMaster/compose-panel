import { useEffect, useRef, useState } from 'react'
import { Link, useNavigate, useParams, useSearchParams } from 'react-router-dom'
import { ago, bytes, del, get, post, put } from '../api'
import { useCan } from '../auth'
import { CodeArea, EnvEditor, EnvVar, langForPath, validateCompose, Validation, ValidationResult } from '../components/editors'
import { Card, Confirm, Empty, ErrorBox, Field, Icon, JobStatus, Loading, Modal, Page, SecretBox, Status, Tabs, useAction, useJobLog, useLoad, useToast } from '../components/ui'
import { MobileCard, ResponsiveTable, joinInline } from '../components/ResponsiveList'
import { triggerName } from './Overview'
import { sourceName } from './Projects'
import { CronTasks, ImagesTab, SaveTemplateModal } from './ProjectExtras'
import { ProjectMetrics } from '../components/charts'

type Tab = 'overview' | 'metrics' | 'deployments' | 'config' | 'files' | 'images' | 'cron' | 'settings'

export default function ProjectDetail() {
  const { id } = useParams()
  const [params, setParams] = useSearchParams()
  const tab = (params.get('tab') as Tab) || 'overview'
  const { data, error, reload } = useLoad(() => get<any>(`/projects/${id}`), [id], 8000)
  const can = useCan()
  const openJob = useJobLog()
  const { busy, run } = useAction()
  const [saveTpl, setSaveTpl] = useState(false)
  const [recreateTarget, setRecreateTarget] = useState<{ service?: string } | null>(null)

  if (!data) return <Page title="项目" crumbs={<Link to="/projects">项目</Link>}>{error ? <ErrorBox error={error} /> : <Loading />}</Page>
  const p = data.project
  const isDB = p.kind === 'database'

  const deploy = async () => {
    const r = await run('deploy', () => post(`/projects/${id}/deploy`))
    if (r) {
      openJob(r.jobId, `部署 ${p.name}`)
      reload()
    }
  }
  const action = async (a: string, service = '') => {
    const r = await run(a, () => post(`/projects/${id}/action`, { action: a, service }))
    if (r) {
      openJob(r.jobId)
      setTimeout(reload, 1500)
    }
  }

  const tabs: [Tab, string][] = [['overview', '概览'], ['metrics', '监控'], ['deployments', '部署记录'], ['config', isDB ? 'Compose' : 'Compose 与环境变量']]
  if (p.source === 'inline') tabs.push(['files', '附加文件'])
  tabs.push(['images', '镜像更新'], ['cron', '定时任务'], ['settings', '设置'])
  const services = [...new Set<string>(data.containers.map((c: any) => c.service).filter(Boolean))]

  return (
    <Page
      title={p.name}
      crumbs={<><Link to={isDB ? '/databases' : '/projects'}>{isDB ? '数据库' : '项目'}</Link> / {p.name}</>}
      sub={
        <span className="row wrap">
          <Status s={data.status} />
          <span>{data.running}/{data.containers.length} 个容器运行</span>
          <span>· {sourceName(p.source)}</span>
          {p.description && <span>· {p.description}</span>}
        </span>
      }
      actions={
        can.operator && (
          <>
            {data.status !== 'running' && data.containers.length > 0 && <button className="btn" disabled={!!busy} onClick={() => action('start')}><Icon name="play" />启动</button>}
            {data.running > 0 && <button className="btn" disabled={!!busy} onClick={() => action('restart')}><Icon name="restart" />重启</button>}
            {data.running > 0 && <button className="btn" disabled={!!busy} onClick={() => action('stop')}><Icon name="stop" />停止</button>}
            {isDB ? (
              <Link className="btn btn-primary" to={`/databases`}>管理数据库</Link>
            ) : (
              <>
                <button className="btn" disabled={!!busy} title="强制拉取最新镜像并重建全部容器" onClick={() => setRecreateTarget({})}><Icon name="refresh" />重建</button>
                {p.source === 'inline' && <button className="btn" onClick={() => setSaveTpl(true)}><Icon name="template" />保存为模板</button>}
                <button className="btn btn-primary" disabled={busy === 'deploy'} onClick={deploy}><Icon name="rocket" />{busy === 'deploy' ? '提交中…' : '部署'}</button>
              </>
            )}
          </>
        )
      }
    >
      <Tabs tabs={tabs} value={tab} onChange={(t) => setParams(t === 'overview' ? {} : { tab: t }, { replace: true })} />
      {tab === 'overview' && <OverviewTab data={data} onAction={action} onRecreate={(service) => setRecreateTarget({ service })} />}
      {tab === 'metrics' && <ProjectMetrics projectId={id!} />}
      {tab === 'deployments' && <DeploymentsTab id={id!} />}
      {tab === 'images' && <ImagesTab project={p} onDeploy={deploy} />}
      {tab === 'cron' && <CronTasks projectId={p.id} services={services} />}
      {tab === 'config' && <ConfigTab project={p} onSaved={reload} />}
      {tab === 'files' && <FilesTab id={id!} />}
      {tab === 'settings' && <SettingsTab data={data} onSaved={reload} />}
      {saveTpl && <SaveTemplateModal project={p} onClose={() => setSaveTpl(false)} />}
      {recreateTarget && (
        <Confirm
          title={recreateTarget.service ? `重建服务 ${recreateTarget.service}` : `重建项目 ${p.name}`}
          confirmText="强制拉取并重建"
          onClose={() => setRecreateTarget(null)}
          onConfirm={() => action('recreate', recreateTarget.service || '')}
        >
          {recreateTarget.service ? (
            <p className="muted">
              将强制拉取服务 <code className="mono">{recreateTarget.service}</code> 的最新镜像并重建容器（若存在多个副本将一并重建）。容器内未通过数据卷持久化的改动将丢失，期间该服务会短暂中断。
            </p>
          ) : (
            <p className="muted">
              将强制拉取项目 <code className="mono">{p.name}</code> 所有服务的最新镜像并重建全部容器。容器内未通过数据卷持久化的改动将丢失，期间服务会短暂中断。
            </p>
          )}
        </Confirm>
      )}
    </Page>
  )
}

function OverviewTab({ data, onAction, onRecreate }: { data: any; onAction: (a: string, s?: string) => void; onRecreate: (service: string) => void }) {
  const can = useCan()
  const nav = useNavigate()
  const { data: stats } = useLoad(() => get<Record<string, any>>('/containers/stats'), [], 10000)
  const p = data.project
  const isDB = p.kind === 'database'
  return (
    <>
      <div className="grid">
        <div className="card stat">
          <span className="label">最近部署</span>
          {data.lastDeploy ? (
            <>
              <div className="row"><JobStatus s={data.lastDeploy.status} /><Link className="sub-link" to={`/deployments/${data.lastDeploy.id}`}>#{data.lastDeploy.number}</Link></div>
              <span className="label">{triggerName(data.lastDeploy.trigger)} · {data.lastDeploy.actor} · {ago(data.lastDeploy.createdAt)}</span>
            </>
          ) : (
            <span className="muted">尚未部署</span>
          )}
        </div>
        <div className="card stat">
          <span className="label">域名</span>
          {data.domains?.length ? data.domains.map((d: string) => <div key={d}><a className="sub-link mono" href={'https://' + d} target="_blank" rel="noreferrer">{d}</a></div>) : <span className="muted">未绑定 · <Link className="sub-link" to="/sites">添加站点</Link></span>}
        </div>
        <div className="card stat">
          <span className="label">目录</span>
          <span className="mono" style={{ overflowWrap: 'anywhere' }}>{data.dir}</span>
          {p.source === 'git' && <span className="label mono" style={{ overflowWrap: 'anywhere' }}>{p.gitUrl} @ {p.gitBranch}</span>}
        </div>
      </div>
      <Card title="容器" pad={false}>
        {data.containers.length === 0 ? (
          <Empty>还没有容器。点击「部署」启动这个项目。</Empty>
        ) : (
          <ResponsiveTable
            table={
              <div className="table-wrap">
                <table>
                  <thead><tr><th>服务</th><th>状态</th><th>镜像</th><th>CPU</th><th>内存</th><th>端口</th><th></th></tr></thead>
                  <tbody>
                    {data.containers.map((c: any) => {
                      const st = stats?.[c.id]
                      return (
                        <tr key={c.id} className="clickable" onClick={() => nav(`/containers/${c.id}`)}>
                          <td><Link className="row-link" to={`/containers/${c.id}`} onClick={(e) => e.stopPropagation()}>{c.service || c.name}</Link><div className="label mono">{c.name}</div></td>
                          <td><Status s={c.health === 'unhealthy' ? 'unhealthy' : c.state} /><div className="label">{c.status}</div></td>
                          <td className="mono ellipsis" style={{ maxWidth: 240, fontSize: 13 }} title={c.image}>{c.image}</td>
                          <td className="mono">{st ? st.cpuPercent.toFixed(1) + '%' : '—'}</td>
                          <td className="mono nowrap">{st ? bytes(st.memUsage) : '—'}</td>
                          <td className="mono" style={{ fontSize: 12 }}>{portsText(c.ports)}</td>
                          <td onClick={(e) => e.stopPropagation()}>
                            <div className="row" style={{ justifyContent: 'flex-end' }}>
                              <Link className="btn btn-sm" to={`/containers/${c.id}?tab=logs`}>日志</Link>
                              {can.operator && c.state === 'running' && <Link className="btn btn-sm" to={`/containers/${c.id}?tab=terminal`}>终端</Link>}
                              {can.operator && c.service && <button className="btn btn-sm" onClick={() => onAction('restart', c.service)}>重启</button>}
                              {can.operator && !isDB && c.service && <button className="btn btn-sm" title="强制拉取最新镜像并重建该服务容器" onClick={() => onRecreate(c.service)}>重建</button>}
                            </div>
                          </td>
                        </tr>
                      )
                    })}
                  </tbody>
                </table>
              </div>
            }
            cards={data.containers.map((c: any) => {
              const st = stats?.[c.id]
              const ports = portsText(c.ports)
              return (
                <MobileCard
                  key={c.id}
                  onClick={() => nav(`/containers/${c.id}`)}
                  title={<Link className="row-link" to={`/containers/${c.id}`} onClick={(e) => e.stopPropagation()}>{c.service || c.name}</Link>}
                  sub={<span className="mono">{c.name}</span>}
                  badges={<Status s={c.health === 'unhealthy' ? 'unhealthy' : c.state} />}
                  meta={[
                    { label: '镜像', value: c.image, mono: true, full: true },
                    { label: '运行态', value: c.status, full: true },
                    { label: 'CPU', value: st ? st.cpuPercent.toFixed(1) + '%' : '—', mono: true },
                    { label: '内存', value: st ? bytes(st.memUsage) : '—', mono: true },
                    ports !== '—' ? { label: '端口', value: ports, mono: true, full: true } : false,
                  ]}
                  actions={
                    <>
                      <Link className="btn btn-sm" to={`/containers/${c.id}?tab=logs`}>日志</Link>
                      {can.operator && c.state === 'running' && <Link className="btn btn-sm" to={`/containers/${c.id}?tab=terminal`}>终端</Link>}
                      {can.operator && c.service && <button className="btn btn-sm" onClick={() => onAction('restart', c.service)}>重启</button>}
                      {can.operator && !isDB && c.service && <button className="btn btn-sm" onClick={() => onRecreate(c.service)}>重建</button>}
                    </>
                  }
                />
              )
            })}
          />
        )}
      </Card>
    </>
  )
}

export function portsText(ports: any[] | null) {
  if (!ports?.length) return '—'
  const seen = new Set<string>()
  for (const p of ports) seen.add(p.PublicPort ? `${p.PublicPort}→${p.PrivatePort}` : `${p.PrivatePort}/${p.Type}`)
  return [...seen].join(' ')
}

function DeploymentsTab({ id }: { id: string }) {
  const { data, error } = useLoad(() => get<any[]>(`/projects/${id}/deployments`), [id], 8000)
  const openJob = useJobLog()
  const nav = useNavigate()
  if (!data) return error ? <ErrorBox error={error} /> : <Loading />
  return (
    <section className="card">
      {data.length === 0 ? (
        <Empty>还没有部署记录。</Empty>
      ) : (
        <ResponsiveTable
          table={
            <div className="table-wrap">
              <table>
                <thead><tr><th>#</th><th>状态</th><th>触发</th><th>提交</th><th>开始</th><th>耗时</th><th></th></tr></thead>
                <tbody>
                  {data.map((d) => (
                    <tr key={d.id} className="clickable" onClick={() => nav(`/deployments/${d.id}`)}>
                      <td className="mono">#{d.number}</td>
                      <td><JobStatus s={d.status} />{d.error && <div className="label ellipsis" style={{ maxWidth: 260, color: '#A41E14' }} title={d.error}>{d.error}</div>}</td>
                      <td>{triggerName(d.trigger)} <span className="muted">· {d.actor}</span></td>
                      <td className="mono" style={{ fontSize: 12 }}>{d.commitSha ? d.commitSha.slice(0, 8) + ' ' + (d.commitMsg || '').slice(0, 40) : '—'}</td>
                      <td className="muted nowrap">{ago(d.createdAt)}</td>
                      <td className="muted nowrap">{d.finishedAt ? Math.max(0, d.finishedAt - d.createdAt) + ' 秒' : '—'}</td>
                      <td>{d.jobId && <button className="btn btn-sm" onClick={(e) => { e.stopPropagation(); openJob(d.jobId, `部署 #${d.number}`) }}>日志</button>}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          }
          cards={data.map((d) => (
            <MobileCard
              key={d.id}
              onClick={() => nav(`/deployments/${d.id}`)}
              title={<span className="mono">部署 #{d.number}</span>}
              sub={joinInline([triggerName(d.trigger), d.actor])}
              badges={<JobStatus s={d.status} />}
              extra={d.error ? <div className="label" style={{ color: '#A41E14', overflowWrap: 'anywhere' }}>{d.error}</div> : undefined}
              meta={[
                { label: '开始', value: ago(d.createdAt) },
                { label: '耗时', value: d.finishedAt ? Math.max(0, d.finishedAt - d.createdAt) + ' 秒' : '—' },
                d.commitSha ? { label: '提交', value: d.commitSha.slice(0, 8) + ' ' + (d.commitMsg || '').slice(0, 60), mono: true, full: true } : false,
              ]}
              actions={d.jobId ? <button className="btn btn-sm" onClick={() => openJob(d.jobId, `部署 #${d.number}`)}>日志</button> : undefined}
            />
          ))}
        />
      )}
    </section>
  )
}

function ConfigTab({ project, onSaved }: { project: any; onSaved: () => void | Promise<void> }) {
  const can = useCan()
  const toast = useToast()
  const openJob = useJobLog()
  const [compose, setCompose] = useState<string>(project.compose || '')
  const [env, setEnv] = useState<EnvVar[]>(project.env || [])
  const [v, setV] = useState<Validation | null>(null)
  const [busy, setBusy] = useState(false)
  const [dirty, setDirty] = useState(false)
  const dirtyRef = useRef(false)
  const markDirty = (d: boolean) => {
    dirtyRef.current = d
    setDirty(d)
  }
  const editable = can.operator && project.source !== 'git' && project.kind !== 'database'

  useEffect(() => {
    if (!dirtyRef.current) {
      setCompose(project.compose || '')
      setEnv(project.env || [])
    }
  }, [project])

  const save = async (deploy: boolean) => {
    setBusy(true)
    try {
      if (project.source !== 'git') {
        const r = await validateCompose(compose, env.map((e) => (e.secret && !e.value ? { ...e, value: 'x' } : e)), project.name)
        setV(r)
        if (!r?.valid) {
          toast.err(r?.error || 'Compose 校验失败')
          return
        }
      }
      const r = await put(`/projects/${project.id}`, { ...project, compose, env, deploy })
      markDirty(false)
      toast.ok(deploy ? '已保存，开始部署' : '已保存')
      if (r.jobId) openJob(r.jobId, `部署 ${project.name}`)
      await onSaved()
    } catch (e) {
      toast.err(e)
    } finally {
      setBusy(false)
    }
  }

  return (
    <>
      <Card
        title={project.source === 'git' ? `compose（来自 ${project.gitComposePath}）` : 'compose.yml'}
        actions={editable && <button className="btn btn-sm" onClick={async () => setV(await validateCompose(compose, env, project.name))}>校验</button>}
      >
        <div className="col" style={{ gap: 12 }}>
          {project.source === 'git' && <div className="alert run">Git 项目的 Compose 文件来自仓库，修改请提交到 {project.gitBranch} 分支后重新部署。</div>}
          {project.kind === 'database' && <div className="alert run">数据库实例的 Compose 由面板生成，请在数据库页面修改版本、端口和资源限制。</div>}
          <CodeArea label="compose" value={compose} readOnly={!editable} onChange={(x) => { setCompose(x); markDirty(true); setV(null) }} rows={24} />
          <ValidationResult v={v} />
        </div>
      </Card>
      {project.kind !== 'database' && (
        <Card title="环境变量" actions={<span className="label">写入 .panel/panel.env，作为 --env-file 传给 Compose</span>}>
          <EnvEditor env={env} readOnly={!can.operator} onChange={(e) => { setEnv(e); markDirty(true) }} />
        </Card>
      )}
      {can.operator && project.kind !== 'database' && (
        <div className="row wrap" style={{ justifyContent: 'flex-end' }}>
          {dirty && <span className="label">有未保存的修改</span>}
          <button className="btn" disabled={busy || !dirty} onClick={() => save(false)}>保存</button>
          <button className="btn btn-primary" disabled={busy} onClick={() => save(true)}>保存并部署</button>
        </div>
      )}
    </>
  )
}

function FilesTab({ id }: { id: string }) {
  const can = useCan()
  const toast = useToast()
  const { data, reload } = useLoad(() => get<{ path: string; content: string }[]>(`/projects/${id}/files`), [id])
  const [files, setFiles] = useState<{ path: string; content: string }[]>([])
  const [sel, setSel] = useState(0)
  const [creating, setCreating] = useState(false)
  const [newPath, setNewPath] = useState('')
  useEffect(() => {
    if (data) setFiles(data)
  }, [data])
  const createFile = () => {
    const p = newPath.trim()
    if (!p) return
    const existing = files.findIndex((x) => x.path === p)
    if (existing >= 0) {
      setSel(existing)
      setCreating(false)
      return
    }
    setFiles([...files, { path: p, content: '' }])
    setSel(files.length)
    setCreating(false)
  }
  const save = async () => {
    try {
      await put(`/projects/${id}/files`, files)
      toast.ok('文件已保存，下次部署时写入')
      reload()
    } catch (e) {
      toast.err(e)
    }
  }
  const f = files[sel]
  return (
    <>
      <Card
        title="附加文件"
        actions={
          can.operator && (
            <>
              <button className="btn btn-sm" onClick={() => { setNewPath(''); setCreating(true) }}><Icon name="plus" size={16} />新建文件</button>
              <button className="btn btn-sm btn-primary" onClick={save}>保存全部</button>
            </>
          )
        }
      >
        <p className="label" style={{ marginTop: 0 }}>配置文件等会在部署时写到 compose.yml 旁边，可以在 Compose 中用相对路径挂载（如 <code className="mono">./nginx/default.conf</code>）。</p>
        {files.length === 0 ? (
          <Empty>没有附加文件。</Empty>
        ) : (
          <div className="tool-split">
            <div className="col tool-side" style={{ gap: 4 }}>
              {files.map((x, i) => (
                <button key={i} className={'btn btn-sm' + (i === sel ? ' btn-primary' : ' btn-ghost')} style={{ justifyContent: 'flex-start', maxWidth: '100%' }} onClick={() => setSel(i)}>
                  <span className="mono ellipsis" style={{ minWidth: 0 }}>{x.path}</span>
                </button>
              ))}
            </div>
            {f && (
              <div className="tool-main">
                <div className="row wrap">
                  <input className="input mono grow" style={{ minWidth: 'min(200px, 100%)' }} aria-label="文件路径" value={f.path} readOnly={!can.operator} onChange={(e) => setFiles(files.map((x, i) => (i === sel ? { ...x, path: e.target.value } : x)))} />
                  {can.operator && <button className="btn btn-danger btn-sm" onClick={() => { setFiles(files.filter((_, i) => i !== sel)); setSel(0) }}>删除</button>}
                </div>
                <CodeArea key={f.path} label={f.path} lang={langForPath(f.path)} value={f.content} readOnly={!can.operator} onChange={(c) => setFiles(files.map((x, i) => (i === sel ? { ...x, content: c } : x)))} rows={20} />
              </div>
            )}
          </div>
        )}
      </Card>
      {creating && (
        <Modal
          title="新建附加文件"
          onClose={() => setCreating(false)}
          footer={
            <>
              <button className="btn" onClick={() => setCreating(false)}>取消</button>
              <button className="btn btn-primary" disabled={!newPath.trim()} onClick={createFile}>添加</button>
            </>
          }
        >
          <Field label="文件路径" hint="相对于项目目录，例如 nginx/default.conf">
            <input
              className="input mono"
              value={newPath}
              autoFocus
              placeholder="nginx/default.conf"
              onChange={(e) => setNewPath(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === 'Enter') createFile()
              }}
            />
          </Field>
        </Modal>
      )}
    </>
  )
}

function SettingsTab({ data, onSaved }: { data: any; onSaved: () => void }) {
  const p = data.project
  const can = useCan()
  const toast = useToast()
  const nav = useNavigate()
  const openJob = useJobLog()
  const [form, setForm] = useState({ ...p, options: { pull: true, wait: true, waitTimeout: 120, autoRollback: true, ...p.options } })
  const [hook, setHook] = useState<{ webhookUrl: string; secret: string } | null>(null)
  const [deleting, setDeleting] = useState(false)
  const [rmVolumes, setRmVolumes] = useState(false)
  const [rmFiles, setRmFiles] = useState(true)
  const [confirmKey, setConfirmKey] = useState(false)
  const set = (patch: any) => setForm({ ...form, ...patch })
  const setOpt = (patch: any) => setForm({ ...form, options: { ...form.options, ...patch } })

  const save = async () => {
    try {
      await put(`/projects/${p.id}`, { ...form, deploy: false })
      toast.ok('设置已保存')
      onSaved()
    } catch (e) {
      toast.err(e)
    }
  }
  const webhookFull = (u: string) => location.origin + u

  return (
    <>
      <Card title="基本">
        <div className="form-grid">
          <Field label="描述"><input className="input" value={form.description} readOnly={!can.operator} onChange={(e) => set({ description: e.target.value })} /></Field>
          {p.source === 'git' && (
            <>
              <Field label="仓库地址"><input className="input mono" value={form.gitUrl} readOnly={!can.operator} onChange={(e) => set({ gitUrl: e.target.value })} /></Field>
              <Field label="分支"><input className="input mono" value={form.gitBranch} readOnly={!can.operator} onChange={(e) => set({ gitBranch: e.target.value })} /></Field>
              <Field label="Compose 文件路径"><input className="input mono" value={form.gitComposePath} readOnly={!can.operator} onChange={(e) => set({ gitComposePath: e.target.value })} /></Field>
            </>
          )}
        </div>
      </Card>

      {p.kind !== 'database' && (
        <Card title="部署选项">
          <div className="col">
            <label className="check"><input type="checkbox" checked={form.options.pull !== false} disabled={!can.operator} onChange={(e) => setOpt({ pull: e.target.checked })} />部署前拉取最新镜像</label>
            <label className="check"><input type="checkbox" checked={form.options.wait !== false} disabled={!can.operator} onChange={(e) => setOpt({ wait: e.target.checked })} />等待容器健康（--wait）</label>
            <label className="check"><input type="checkbox" checked={form.options.autoRollback !== false} disabled={!can.operator} onChange={(e) => setOpt({ autoRollback: e.target.checked })} />失败时自动回滚到上一次成功部署</label>
            <label className="check"><input type="checkbox" checked={!!form.options.autoUpdate} disabled={!can.operator} onChange={(e) => setOpt({ autoUpdate: e.target.checked })} />镜像有新版本时自动重新部署（按「系统设置」中的镜像检查计划）</label>
            <label className="check"><input type="checkbox" checked={!!form.options.skipNetworks} disabled={!can.operator} onChange={(e) => setOpt({ skipNetworks: e.target.checked })} />不自动接入代理网络（自行管理 networks）</label>
            <Field label="等待超时（秒）"><input className="input" style={{ maxWidth: 160 }} type="number" min={10} value={form.options.waitTimeout || 120} readOnly={!can.operator} onChange={(e) => setOpt({ waitTimeout: Number(e.target.value) })} /></Field>
          </div>
        </Card>
      )}

      {p.source === 'git' && (
        <Card title="Webhook 与部署密钥">
          <div className="col" style={{ gap: 14 }}>
            <label className="check"><input type="checkbox" checked={form.autoDeploy} disabled={!can.operator} onChange={(e) => set({ autoDeploy: e.target.checked })} />推送到 {form.gitBranch} 时自动部署</label>
            {p.webhookUrl && <SecretBox label="Webhook 地址（GitHub / Gitea / GitLab，Content type 选 application/json）" value={webhookFull(hook?.webhookUrl || p.webhookUrl)} />}
            {hook && <SecretBox label="签名密钥（只显示这一次，填到仓库的 Secret / Token 中）" value={hook.secret} />}
            {!hook && <span className="label">{data.hasWebhookSecret ? '已设置签名密钥。' : '尚未设置签名密钥，建议生成。'}</span>}
            {can.operator && (
              <div className="row wrap">
                <button className="btn btn-sm" onClick={async () => { try { setHook(await post(`/projects/${p.id}/webhook/rotate`)); toast.ok('已生成新的 Webhook 地址和密钥') } catch (e) { toast.err(e) } }}>重新生成 Webhook 地址与密钥</button>
              </div>
            )}
            {p.deployKey && (
              <>
                <SecretBox label="部署公钥（添加到仓库的 Deploy keys，只读即可）" value={p.deployKey} />
                {can.operator && <div><button className="btn btn-sm" onClick={() => setConfirmKey(true)}>重新生成部署密钥</button></div>}
              </>
            )}
          </div>
        </Card>
      )}

      {can.operator && (
        <div className="row" style={{ justifyContent: 'flex-end' }}>
          <button className="btn btn-primary" onClick={save}>保存设置</button>
        </div>
      )}

      {can.admin && p.kind !== 'database' && (
        <Card title="危险操作">
          <div className="row wrap">
            <div className="grow">
              <div style={{ fontWeight: 600 }}>删除项目</div>
              <div className="label">停止并删除全部容器（compose down），删除面板中的记录和站点。</div>
            </div>
            <button className="btn btn-danger" onClick={() => setDeleting(true)}>删除项目…</button>
          </div>
        </Card>
      )}

      {confirmKey && (
        <Confirm title="重新生成部署密钥" onClose={() => setConfirmKey(false)} onConfirm={async () => { await post(`/projects/${p.id}/deploy-key`); toast.ok('已生成新密钥，请更新仓库中的 Deploy key'); onSaved() }}>
          旧的公钥将失效，需要把新公钥重新添加到仓库。
        </Confirm>
      )}
      {deleting && (
        <Confirm
          title={`删除项目 ${p.name}`}
          danger
          confirmText="删除"
          typeToConfirm={p.name}
          onClose={() => setDeleting(false)}
          onConfirm={async () => {
            const r = await del(`/projects/${p.id}?volumes=${rmVolumes ? 1 : 0}&files=${rmFiles ? 1 : 0}`)
            openJob(r.jobId, `删除 ${p.name}`)
            nav('/projects')
          }}
        >
          <label className="check"><input type="checkbox" checked={rmVolumes} onChange={(e) => setRmVolumes(e.target.checked)} />同时删除数据卷（不可恢复）</label>
          <label className="check"><input type="checkbox" checked={rmFiles} onChange={(e) => setRmFiles(e.target.checked)} />删除项目目录中的文件</label>
        </Confirm>
      )}
    </>
  )
}

