import { useEffect, useState } from 'react'
import { Link, useNavigate, useParams } from 'react-router-dom'
import { dateTime, get, post, streamLines } from '../api'
import { useCan } from '../auth'
import { Diff } from '../components/editors'
import { Card, Confirm, ErrorBox, JobStatus, Loading, LogView, Page, Tabs, useJobLog, useLoad } from '../components/ui'
import { triggerName } from './Overview'

export default function DeploymentDetail() {
  const { id } = useParams()
  const { data, error, reload } = useLoad(() => get<any>(`/deployments/${id}`), [id])
  const can = useCan()
  const nav = useNavigate()
  const openJob = useJobLog()
  const [tab, setTab] = useState<'log' | 'diff' | 'compose' | 'env'>('log')
  const [lines, setLines] = useState<string[]>([])
  const [rollback, setRollback] = useState(false)
  const d = data?.deployment

  useEffect(() => {
    if (!d?.jobId) return
    const buf: string[] = []
    let raf = 0
    setLines([])
    return streamLines(`/jobs/${d.jobId}/log`, (l) => {
      buf.push(l)
      if (!raf) raf = requestAnimationFrame(() => { raf = 0; setLines([...buf]) })
    }, () => reload())
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [d?.jobId])

  if (!data) return <Page title="部署">{error ? <ErrorBox error={error} /> : <Loading />}</Page>
  const p = data.project
  return (
    <Page
      title={`${p.name} · 部署 #${d.number}`}
      crumbs={<><Link to="/projects">项目</Link> / <Link to={`/projects/${p.id}?tab=deployments`}>{p.name}</Link> / #{d.number}</>}
      sub={<span className="row wrap"><JobStatus s={d.status} /><span>{triggerName(d.trigger)} · {d.actor} · {dateTime(d.createdAt)}</span>{d.commitSha && <span className="mono">{d.commitSha.slice(0, 10)} {d.commitMsg}</span>}</span>}
      actions={can.operator && data.compose && d.status === 'success' && <button className="btn" onClick={() => setRollback(true)}>回滚到此版本</button>}
    >
      {d.error && <div className="alert err">{d.error}</div>}
      <Tabs tabs={[['log', '部署日志'], ['diff', '与上次对比'], ['compose', 'Compose 快照'], ['env', '环境变量快照']]} value={tab} onChange={setTab} />
      {tab === 'log' && (
        <section className="card" style={{ overflow: 'hidden' }}>
          {d.jobId ? <LogView lines={lines} height={560} /> : <div className="empty">没有日志。</div>}
        </section>
      )}
      {tab === 'diff' && (
        <Card title={data.previousId ? `对比部署 ${data.previousId} → ${d.id}` : '这是第一次带快照的部署'} pad={false}>
          <Diff a={data.previousCompose || ''} b={data.compose || ''} />
        </Card>
      )}
      {tab === 'compose' && (
        <Card pad={false}>
          <pre className="diff" style={{ margin: 0, padding: 16, maxHeight: 600 }}>{data.compose || '（无快照）'}</pre>
        </Card>
      )}
      {tab === 'env' && (
        <Card pad={false}>
          <div className="table-wrap">
            <table>
              <thead><tr><th>变量</th><th>值</th></tr></thead>
              <tbody>
                {data.env.length === 0 && <tr><td colSpan={2} className="muted">没有环境变量</td></tr>}
                {data.env.map((e: any) => <tr key={e.key}><td className="mono">{e.key}</td><td className="mono">{e.value}</td></tr>)}
              </tbody>
            </table>
          </div>
        </Card>
      )}
      {rollback && (
        <Confirm
          title={`回滚到部署 #${d.number}`}
          confirmText="开始回滚"
          onClose={() => setRollback(false)}
          onConfirm={async () => {
            const r = await post(`/deployments/${id}/rollback`)
            openJob(r.jobId, `回滚 ${p.name}`)
            nav(`/deployments/${r.deploymentId}`)
          }}
        >
          使用部署 #{d.number} 的 Compose 与环境变量快照重新部署。数据卷不会回滚。
        </Confirm>
      )}
    </Page>
  )
}
