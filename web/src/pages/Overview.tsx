import { Link } from 'react-router-dom'
import { ago, bytes, duration, get, pct } from '../api'
import { Card, Empty, ErrorBox, JobStatus, Loading, Meter, Page, Status, useJobLog, useLoad } from '../components/ui'

export default function Overview() {
  const { data: o, error } = useLoad(() => get<any>('/overview'), [], 15000)
  const openJob = useJobLog()
  if (!o) return <Page title="总览">{error ? <ErrorBox error={error} /> : <Loading />}</Page>
  const h = o.host || {}
  const issues: { text: string; to: string }[] = []
  if (!o.docker) issues.push({ text: '无法连接 Docker：' + (o.dockerError || ''), to: '/settings' })
  for (const c of o.unhealthy || []) issues.push({ text: `容器 ${c.name} ${c.health === 'unhealthy' ? '健康检查失败' : c.status}`, to: '/containers/' + c.id })
  for (const b of o.backups?.recentFailures || []) issues.push({ text: `备份失败：${b.label}（${ago(b.startedAt)}）`, to: '/backups' })
  for (const c of o.certs || []) if (c.daysLeft <= 14) issues.push({ text: `证书 ${c.domains?.[0]} 将在 ${c.daysLeft} 天后过期`, to: '/certificates' })

  return (
    <Page title="总览" sub={h.uptime ? `主机已运行 ${duration(h.uptime)}` : undefined}>
      {issues.length > 0 && (
        <Card title={`需要关注（${issues.length}）`} pad={false}>
          <div className="table-wrap">
            <table>
              <tbody>
                {issues.slice(0, 8).map((i, k) => (
                  <tr key={k}>
                    <td><span className="badge err"><span className="dot" />问题</span></td>
                    <td className="grow">{i.text}</td>
                    <td style={{ textAlign: 'right' }}><Link to={i.to}>查看</Link></td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </Card>
      )}

      <div className="grid">
        <div className="card stat">
          <span className="label">CPU · {h.cpus || '—'} 核</span>
          <div className="big">{h.cpuPercent != null ? h.cpuPercent.toFixed(0) : '—'}<small> %</small></div>
          <Meter value={h.cpuPercent || 0} max={100} />
          <span className="label">负载 {(h.load || []).map((x: number) => x.toFixed(2)).join(' / ') || '—'}</span>
        </div>
        <div className="card stat">
          <span className="label">内存</span>
          <div className="big">{pct(h.memUsed, h.memTotal)}<small> %</small></div>
          <Meter value={h.memUsed} max={h.memTotal} />
          <span className="label">{bytes(h.memUsed)} / {bytes(h.memTotal)}</span>
        </div>
        <div className="card stat">
          <span className="label">磁盘（数据目录）</span>
          <div className="big">{pct(h.diskUsed, h.diskTotal)}<small> %</small></div>
          <Meter value={h.diskUsed} max={h.diskTotal} />
          <span className="label">{bytes(h.diskUsed)} / {bytes(h.diskTotal)}</span>
        </div>
        <div className="card stat">
          <span className="label">容器</span>
          <div className="big">{o.containers.running}<small> / {o.containers.total} 运行中</small></div>
          <span className="label">{o.containers.unhealthy ? <span style={{ color: '#A41E14' }}>{o.containers.unhealthy} 个异常</span> : '全部正常'}</span>
          <span className="label">24 小时备份：{o.backups.success} 成功 · {o.backups.failed} 失败 · {bytes(o.backups.size)}</span>
        </div>
      </div>

      <div className="grid-2">
        <Card title="项目" actions={<Link className="btn btn-sm" to="/projects">全部项目</Link>} pad={false}>
          {o.projects.length === 0 ? (
            <Empty action={<Link className="btn btn-primary" to="/projects/new">新建项目</Link>}>还没有项目。</Empty>
          ) : (
            <div className="table-wrap">
              <table>
                <thead><tr><th>名称</th><th>状态</th><th>容器</th></tr></thead>
                <tbody>
                  {o.projects.slice(0, 10).map((p: any) => (
                    <tr key={p.id}>
                      <td><Link to={`/projects/${p.id}`}>{p.name}</Link>{p.kind === 'database' && <span className="tag" style={{ marginLeft: 8 }}>数据库</span>}</td>
                      <td><Status s={p.status} /></td>
                      <td className="mono">{p.running}/{p.total}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </Card>

        <Card title="最近部署" pad={false}>
          {o.deployments.length === 0 ? (
            <Empty>还没有部署记录。</Empty>
          ) : (
            <div className="table-wrap">
              <table>
                <thead><tr><th>项目</th><th>状态</th><th>触发</th><th>时间</th></tr></thead>
                <tbody>
                  {o.deployments.map((d: any) => (
                    <tr key={d.deployment.id}>
                      <td><Link to={`/deployments/${d.deployment.id}`}>{d.project} #{d.deployment.number}</Link></td>
                      <td><JobStatus s={d.deployment.status} /></td>
                      <td className="muted">{triggerName(d.deployment.trigger)} · {d.deployment.actor}</td>
                      <td className="muted nowrap">{ago(d.deployment.createdAt)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </Card>

        <Card title="最近任务" actions={<Link className="btn btn-sm" to="/jobs">任务中心</Link>} pad={false}>
          {o.jobs.length === 0 ? (
            <Empty>最近 6 小时没有任务。</Empty>
          ) : (
            <div className="table-wrap">
              <table>
                <tbody>
                  {o.jobs.map((j: any) => (
                    <tr key={j.id} className="clickable" onClick={() => openJob(j.id, j.title)}>
                      <td>{j.title}</td>
                      <td><JobStatus s={j.status} /></td>
                      <td className="muted nowrap">{ago(j.createdAt)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </Card>

        <Card title="证书" actions={<Link className="btn btn-sm" to="/certificates">全部证书</Link>} pad={false}>
          {o.certs.length === 0 ? (
            <Empty>近期没有即将过期的证书。</Empty>
          ) : (
            <div className="table-wrap">
              <table>
                <tbody>
                  {o.certs.map((c: any) => (
                    <tr key={c.id + c.name}>
                      <td className="mono">{c.domains?.join(', ')}</td>
                      <td><span className={'badge ' + (c.daysLeft <= 7 ? 'err' : 'warn')}>剩 {c.daysLeft} 天</span></td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </Card>
      </div>
    </Page>
  )
}

export function triggerName(t: string) {
  return ({ manual: '手动', webhook: 'Webhook', rollback: '回滚', create: '创建', api: 'API', import: '导入' } as Record<string, string>)[t] || t
}
