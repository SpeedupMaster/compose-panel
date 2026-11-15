import { useState } from 'react'
import { ago, dateTime, del, get, post, put } from '../api'
import { useCan } from '../auth'
import { Card, Confirm, Empty, ErrorBox, Field, Icon, JobStatus, Loading, Modal, Select, useAction, useJobLog, useLoad, useToast } from '../components/ui'
import { MobileCard, ResponsiveTable } from '../components/ResponsiveList'

// ---------- image updates ----------

type ImageCheck = { projectId: number; project: string; service: string; image: string; localDigest: string; remoteDigest: string; status: string; error: string; checkedAt: number }

const imageStatus: Record<string, [string, string]> = {
  current: ['ok', '已是最新'],
  update: ['run', '有新版本'],
  local: ['off', '本地镜像'],
  pinned: ['off', '固定 digest'],
  error: ['err', '检查失败'],
}

const short = (d: string) => (d ? d.replace(/^sha256:/, '').slice(0, 12) : '—')

export function ImagesTab({ project, onDeploy }: { project: any; onDeploy: () => void }) {
  const can = useCan()
  const { data, error, setData } = useLoad(() => get<ImageCheck[]>(`/projects/${project.id}/images`), [project.id])
  const { busy, run } = useAction()
  const check = async () => {
    const r = await run('check', () => post<ImageCheck[]>(`/projects/${project.id}/images/check`), '检查完成')
    if (r) setData(r)
  }
  const updates = (data || []).filter((c) => c.status === 'update').length
  return (
    <>
      {updates > 0 && (
        <div className="alert run" style={{ flexWrap: 'wrap' }}>
          <Icon name="alert" />
          <div className="grow" style={{ minWidth: 'min(220px, 100%)' }}>有 {updates} 个服务的镜像有新版本。重新部署（开启「部署前拉取最新镜像」）即可更新。</div>
          {can.operator && <button className="btn btn-sm btn-primary" onClick={onDeploy}><Icon name="rocket" size={16} />部署更新</button>}
        </div>
      )}
      <Card
        title="镜像版本"
        pad={false}
        actions={
          <div className="row wrap">
            <span className="label">{project.options?.autoUpdate ? '已开启自动更新' : '自动更新未开启（在「设置」中开启）'}</span>
            {can.operator && <button className="btn btn-sm" disabled={!!busy} onClick={check}><Icon name="refresh" size={16} />{busy ? '检查中…' : '立即检查'}</button>}
          </div>
        }
      >
        {error ? <div className="card-body"><ErrorBox error={error} /></div> : !data ? <Loading /> : data.length === 0 ? (
          <Empty>还没有检查记录。面板会按「设置」中的计划定期对比镜像仓库的 digest。</Empty>
        ) : (
          <ResponsiveTable
            table={
              <div className="table-wrap">
                <table>
                  <thead><tr><th>服务</th><th>镜像</th><th>状态</th><th>本地</th><th>仓库</th><th>检查时间</th></tr></thead>
                  <tbody>
                    {data.map((c) => {
                      const [k, t] = imageStatus[c.status] || ['off', c.status]
                      return (
                        <tr key={c.service}>
                          <td style={{ fontWeight: 600 }}>{c.service}</td>
                          <td className="mono" style={{ fontSize: 13, overflowWrap: 'anywhere' }}>{c.image}</td>
                          <td><span className={'badge ' + k}>{t}</span>{c.error && <div className="label" style={{ color: '#A41E14', maxWidth: 280 }}>{c.error}</div>}</td>
                          <td className="mono" title={c.localDigest}>{short(c.localDigest)}</td>
                          <td className="mono" title={c.remoteDigest}>{short(c.remoteDigest)}</td>
                          <td className="muted nowrap">{ago(c.checkedAt)}</td>
                        </tr>
                      )
                    })}
                  </tbody>
                </table>
              </div>
            }
            cards={data.map((c) => {
              const [k, t] = imageStatus[c.status] || ['off', c.status]
              return (
                <MobileCard
                  key={c.service}
                  title={c.service}
                  sub={<span className="mono">{c.image}</span>}
                  badges={<span className={'badge ' + k}>{t}</span>}
                  extra={c.error ? <div className="label" style={{ color: '#A41E14', overflowWrap: 'anywhere' }}>{c.error}</div> : undefined}
                  meta={[
                    { label: '本地', value: short(c.localDigest), mono: true },
                    { label: '仓库', value: short(c.remoteDigest), mono: true },
                    { label: '检查时间', value: ago(c.checkedAt), full: true },
                  ]}
                />
              )
            })}
          />
        )}
      </Card>
    </>
  )
}

// ---------- save as template ----------

export function SaveTemplateModal({ project, onClose }: { project: any; onClose: () => void }) {
  const [name, setName] = useState(project.name)
  const [description, setDescription] = useState(project.description || '')
  const [keepSecrets, setKeepSecrets] = useState(false)
  const { busy, run } = useAction()
  const save = async () => {
    const r = await run('save', () => post(`/projects/${project.id}/template`, { name, description, keepSecrets }), '已保存为模板，新建项目时可以选择')
    if (r) onClose()
  }
  return (
    <Modal
      title="保存为模板"
      onClose={onClose}
      footer={<><button className="btn" onClick={onClose}>取消</button><button className="btn btn-primary" disabled={!!busy || !name.trim()} onClick={save}>保存</button></>}
    >
      <p className="label" style={{ margin: 0 }}>保存当前的 compose.yml、环境变量和附加文件。Compose 中的 <code className="mono">${'{VAR}'}</code> 会作为模板变量，新建项目时填写。</p>
      <Field label="模板名称"><input className="input" value={name} onChange={(e) => setName(e.target.value)} autoFocus /></Field>
      <Field label="描述"><input className="input" value={description} onChange={(e) => setDescription(e.target.value)} /></Field>
      <label className="check"><input type="checkbox" checked={keepSecrets} onChange={(e) => setKeepSecrets(e.target.checked)} />保留敏感变量的值（默认清空，使用模板时自动生成随机值）</label>
    </Modal>
  )
}

// ---------- cron tasks ----------

export type CronTask = {
  id: number
  name: string
  projectId: number
  projectName: string
  service: string
  command: string
  user: string
  schedule: string
  timeout: number
  enabled: boolean
  notify: boolean
  lastRunAt: number
  lastStatus: string
  lastJobId: string
  next: number
}

export function CronTasks({ projectId, services }: { projectId?: number; services?: string[] }) {
  const can = useCan()
  const openJob = useJobLog()
  const toast = useToast()
  const { data, error, reload } = useLoad(() => get<CronTask[]>('/cron-tasks' + (projectId ? `?project=${projectId}` : '')), [projectId], 15000)
  const [edit, setEdit] = useState<Partial<CronTask> | null>(null)
  const [rm, setRm] = useState<CronTask | null>(null)
  const runNow = async (t: CronTask) => {
    try {
      const r = await post(`/cron-tasks/${t.id}/run`)
      openJob(r.jobId, `定时任务 ${t.name}`)
      setTimeout(reload, 1000)
    } catch (e) {
      toast.err(e)
    }
  }
  const toggle = async (t: CronTask) => {
    try {
      await put(`/cron-tasks/${t.id}`, { ...t, enabled: !t.enabled })
      reload()
    } catch (e) {
      toast.err(e)
    }
  }
  return (
    <Card
      title="定时任务"
      pad={false}
      actions={can.operator && <button className="btn btn-sm" onClick={() => setEdit({ projectId, schedule: '0 3 * * *', timeout: 600, enabled: true, notify: true, service: services?.[0] || '' })}><Icon name="plus" size={16} />新建任务</button>}
    >
      {error ? <div className="card-body"><ErrorBox error={error} /></div> : !data ? <Loading /> : data.length === 0 ? (
        <Empty>没有定时任务。可以在服务容器内按计划执行命令，例如清理缓存、生成报表、调用 artisan / manage.py。</Empty>
      ) : (
        <ResponsiveTable
          table={
            <div className="table-wrap">
              <table>
                <thead><tr><th>名称</th>{!projectId && <th>项目</th>}<th>服务 / 命令</th><th>计划</th><th>上次执行</th><th>下次</th><th /></tr></thead>
                <tbody>
                  {data.map((t) => (
                    <tr key={t.id}>
                      <td style={{ fontWeight: 600 }}>{t.name}{!t.enabled && <span className="badge off" style={{ marginLeft: 6 }}>已停用</span>}</td>
                      {!projectId && <td>{t.projectName}</td>}
                      <td><span className="tag">{t.service}</span> <span className="mono ellipsis" style={{ display: 'inline-block', maxWidth: 280, verticalAlign: 'middle', fontSize: 12.5 }} title={t.command}>{t.command}</span></td>
                      <td className="mono nowrap">{t.schedule}</td>
                      <td className="nowrap">
                        {t.lastRunAt ? (
                          <button className="link-btn" style={{ textDecoration: 'none' }} disabled={!t.lastJobId} onClick={() => openJob(t.lastJobId, `定时任务 ${t.name}`)}>
                            <JobStatus s={t.lastStatus} /> <span className="label">{ago(t.lastRunAt)}</span>
                          </button>
                        ) : <span className="muted">—</span>}
                      </td>
                      <td className="muted nowrap" title={t.next ? dateTime(t.next) : ''}>{t.enabled && t.next ? ago(t.next) : '—'}</td>
                      <td>
                        {can.operator && (
                          <div className="row" style={{ justifyContent: 'flex-end' }}>
                            <button className="btn btn-sm" onClick={() => runNow(t)}><Icon name="play" size={14} />执行</button>
                            <button className="btn btn-sm" onClick={() => toggle(t)}>{t.enabled ? '停用' : '启用'}</button>
                            <button className="btn btn-sm btn-ghost btn-icon" aria-label="编辑" onClick={() => setEdit(t)}><Icon name="edit" size={16} /></button>
                            <button className="btn btn-sm btn-ghost btn-icon" aria-label="删除" onClick={() => setRm(t)}><Icon name="trash" size={16} /></button>
                          </div>
                        )}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          }
          cards={data.map((t) => (
            <MobileCard
              key={t.id}
              title={
                <>
                  <span>{t.name}</span>
                  <span className="tag">{t.service}</span>
                </>
              }
              sub={(!projectId && t.projectName ? `${t.projectName} · ` : '') + t.command}
              badges={!t.enabled ? <span className="badge off">已停用</span> : t.lastStatus ? <JobStatus s={t.lastStatus} /> : undefined}
              meta={[
                { label: '计划', value: t.schedule, mono: true },
                { label: '下次', value: t.enabled && t.next ? ago(t.next) : '—' },
                t.lastRunAt
                  ? {
                      label: '上次执行',
                      value: (
                        <button className="link-btn" style={{ textDecoration: 'none' }} disabled={!t.lastJobId} onClick={() => openJob(t.lastJobId, `定时任务 ${t.name}`)}>
                          <JobStatus s={t.lastStatus} /> <span className="label">{ago(t.lastRunAt)}</span>
                        </button>
                      ),
                      full: true,
                    }
                  : false,
              ]}
              actions={
                can.operator ? (
                  <>
                    <button className="btn btn-sm" onClick={() => runNow(t)}><Icon name="play" size={14} />执行</button>
                    <button className="btn btn-sm" onClick={() => toggle(t)}>{t.enabled ? '停用' : '启用'}</button>
                    <button className="btn btn-sm btn-ghost btn-icon" aria-label="编辑" onClick={() => setEdit(t)}><Icon name="edit" size={16} /></button>
                    <button className="btn btn-sm btn-ghost btn-icon" aria-label="删除" onClick={() => setRm(t)}><Icon name="trash" size={16} /></button>
                  </>
                ) : undefined
              }
            />
          ))}
        />
      )}
      {edit && <CronForm task={edit} services={services} onClose={() => setEdit(null)} onSaved={() => { setEdit(null); reload() }} />}
      {rm && <Confirm title={`删除定时任务 ${rm.name}`} danger confirmText="删除" onClose={() => setRm(null)} onConfirm={async () => { await del(`/cron-tasks/${rm.id}`); reload() }}>删除后不再执行，历史执行记录保留在任务中心。</Confirm>}
    </Card>
  )
}

function CronForm({ task, services, onClose, onSaved }: { task: Partial<CronTask>; services?: string[]; onClose: () => void; onSaved: () => void }) {
  const [t, setT] = useState<Partial<CronTask>>(task)
  const { data: projects } = useLoad(async () => (task.projectId ? null : await get<any[]>('/projects')), [])
  const { data: proj } = useLoad(async () => (t.projectId && !services ? await get<any>(`/projects/${t.projectId}`) : null), [t.projectId])
  const svcList: string[] = services || (proj ? [...new Set<string>((proj.containers || []).map((c: any) => c.service).filter(Boolean))] : [])
  const set = (p: Partial<CronTask>) => setT({ ...t, ...p })
  const { busy, run } = useAction()
  const save = async () => {
    const r = await run('save', () => (t.id ? put(`/cron-tasks/${t.id}`, t) : post('/cron-tasks', t)), '已保存')
    if (r) onSaved()
  }
  return (
    <Modal
      title={t.id ? '编辑定时任务' : '新建定时任务'}
      onClose={onClose}
      footer={<><button className="btn" onClick={onClose}>取消</button><button className="btn btn-primary" disabled={!!busy} onClick={save}>保存</button></>}
    >
      <Field label="名称"><input className="input" value={t.name || ''} onChange={(e) => set({ name: e.target.value })} autoFocus /></Field>
      {!task.projectId && (
        <Field label="项目">
          <Select value={t.projectId || ''} onChange={(v) => set({ projectId: Number(v), service: '' })}>
            <option value="">选择项目</option>
            {(projects || []).map((p: any) => <option key={p.id} value={p.id}>{p.name}</option>)}
          </Select>
        </Field>
      )}
      <div className="form-grid">
        <Field label="服务" hint="在该服务第一个运行中的容器里执行">
          {svcList.length ? (
            <Select value={t.service || ''} onChange={(v) => set({ service: v })}>
              <option value="">选择服务</option>
              {svcList.map((s) => <option key={s}>{s}</option>)}
            </Select>
          ) : <input className="input mono" value={t.service || ''} onChange={(e) => set({ service: e.target.value })} />}
        </Field>
        <Field label="执行用户" hint="留空使用容器默认用户"><input className="input mono" value={t.user || ''} onChange={(e) => set({ user: e.target.value })} placeholder="如 www-data" /></Field>
      </div>
      <Field label="命令" hint="通过 sh -c 执行"><textarea className="textarea code" rows={3} value={t.command || ''} onChange={(e) => set({ command: e.target.value })} placeholder="php artisan schedule:run" /></Field>
      <div className="form-grid">
        <Field label="Cron 表达式" hint="分 时 日 月 周，例如 */10 * * * *"><input className="input mono" value={t.schedule || ''} onChange={(e) => set({ schedule: e.target.value })} /></Field>
        <Field label="超时（秒）"><input className="input" type="number" min={10} value={t.timeout || 600} onChange={(e) => set({ timeout: Number(e.target.value) })} /></Field>
      </div>
      <label className="check"><input type="checkbox" checked={!!t.enabled} onChange={(e) => set({ enabled: e.target.checked })} />启用</label>
      <label className="check"><input type="checkbox" checked={!!t.notify} onChange={(e) => set({ notify: e.target.checked })} />失败时发送通知</label>
    </Modal>
  )
}
