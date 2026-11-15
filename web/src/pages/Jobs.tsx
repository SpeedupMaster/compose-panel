import { useState } from 'react'
import { ago, dateTime, get, post } from '../api'
import { useCan } from '../auth'
import { CronTasks } from './ProjectExtras'
import { Card, Empty, ErrorBox, JobStatus, Loading, Page, Tabs, useJobLog, useLoad, useToast } from '../components/ui'

export default function Jobs() {
  const [tab, setTab] = useState<'jobs' | 'cron' | 'schedules'>('jobs')
  return (
    <Page title="任务中心" sub="部署、备份、恢复等耗时操作都在这里排队执行">
      <Tabs tabs={[['jobs', '任务'], ['cron', '容器定时任务'], ['schedules', '全部计划']]} value={tab} onChange={setTab} />
      {tab === 'jobs' && <JobList />}
      {tab === 'cron' && <CronTasks />}
      {tab === 'schedules' && <Schedules />}
    </Page>
  )
}

const kinds: [string, string][] = [['', '全部类型'], ['deploy', '部署'], ['backup', '备份'], ['project', '项目操作'], ['database', '数据库'], ['cron', '定时任务'], ['images', '镜像检查'], ['system', '系统']]

function JobList() {
  const [status, setStatus] = useState('')
  const [kind, setKind] = useState('')
  const { data, error } = useLoad(() => get<any[]>(`/jobs?limit=200${status ? '&status=' + status : ''}${kind ? '&kind=' + kind : ''}`), [status, kind], 4000)
  const openJob = useJobLog()
  return (
    <section className="card">
      <div className="card-head">
        <div className="row wrap">
          <select className="select" style={{ width: 140 }} aria-label="状态" value={status} onChange={(e) => setStatus(e.target.value)}>
            <option value="">全部状态</option>
            <option value="active">进行中/排队</option>
            <option value="success">成功</option>
            <option value="failed">失败</option>
            <option value="canceled">已取消</option>
          </select>
          <select className="select" style={{ width: 140 }} aria-label="类型" value={kind} onChange={(e) => setKind(e.target.value)}>
            {kinds.map(([k, l]) => <option key={k} value={k}>{l}</option>)}
          </select>
        </div>
      </div>
      <ErrorBox error={error} />
      {!data ? <Loading /> : data.length === 0 ? <Empty>没有任务。</Empty> : (
        <div className="table-wrap">
          <table>
            <thead><tr><th>任务</th><th>状态</th><th>发起人</th><th>创建</th><th>耗时</th></tr></thead>
            <tbody>
              {data.map((j) => (
                <tr key={j.id} className="clickable" onClick={() => openJob(j.id, j.title)}>
                  <td><div style={{ fontWeight: 500 }}>{j.title}</div><div className="label mono">{j.kind}</div></td>
                  <td><JobStatus s={j.status} />{j.error && <div className="label ellipsis" style={{ maxWidth: 320, color: '#A41E14' }} title={j.error}>{j.error}</div>}</td>
                  <td>{j.actor}</td>
                  <td className="muted nowrap" title={dateTime(j.createdAt)}>{ago(j.createdAt)}</td>
                  <td className="muted nowrap">{j.finishedAt && j.startedAt ? `${j.finishedAt - j.startedAt} 秒` : j.status === 'running' ? '…' : '—'}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  )
}

function Schedules() {
  const { data, error } = useLoad(() => get<any>('/schedules'), [], 30000)
  const can = useCan()
  const toast = useToast()
  const openJob = useJobLog()
  if (!data) return error ? <ErrorBox error={error} /> : <Loading />
  return (
    <Card title={`定时计划（时区 ${data.timezone}）`} pad={false}>
      <div className="table-wrap">
        <table>
          <thead><tr><th>名称</th><th>类型</th><th>计划</th><th>上次</th><th>下次</th><th></th></tr></thead>
          <tbody>
            {data.items.map((s: any) => (
              <tr key={s.key}>
                <td>{s.name}{s.target && <div className="label">{s.target}</div>}</td>
                <td>{({ backup: '备份策略', cron: '容器定时任务', drill: '恢复演练' } as Record<string, string>)[s.kind] || '系统'}</td>
                <td className="mono">{s.spec}</td>
                <td className="muted nowrap">{s.prev ? ago(s.prev) : '—'}</td>
                <td className="muted nowrap">{s.next ? dateTime(s.next) : '—'}</td>
                <td>{can.operator && <button className="btn btn-sm" onClick={async () => { try { const r = await post(`/schedules/${encodeURIComponent(s.key)}/run`); if (r.jobId) openJob(r.jobId, s.name); else toast.ok('已执行') } catch (e) { toast.err(e) } }}>立即运行</button>}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </Card>
  )
}
