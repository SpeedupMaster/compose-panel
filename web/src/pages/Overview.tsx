import { Link, useNavigate } from 'react-router-dom'
import { ago, bytes, duration, get, pct } from '../api'
import { MetricsPanel } from '../components/charts'
import { Card, Empty, ErrorBox, JobStatus, Loading, Meter, Page, Status, useJobLog, useLoad } from '../components/ui'
import { MobileCard, ResponsiveTable, joinInline } from '../components/ResponsiveList'
import { triggerName } from '../components/shared'

// Rows shown per list card; the API returns at most 8 deployments and jobs.
const ROWS = 8

export default function Overview() {
  const { data: o, error } = useLoad(() => get<any>('/overview'), [], 15000)
  const openJob = useJobLog()
  const nav = useNavigate()
  const { data: updates } = useLoad(() => get<any[]>('/image-updates'), [], 300000)
  if (!o) return <Page title="总览">{error ? <ErrorBox error={error} /> : <Loading />}</Page>
  const h = o.host || {}
  const issues: { text: string; to: string; kind?: 'info' }[] = []
  if (!o.docker) issues.push({ text: '无法连接 Docker：' + (o.dockerError || ''), to: '/settings' })
  for (const c of o.unhealthy || []) issues.push({ text: `容器 ${c.name} ${c.health === 'unhealthy' ? '健康检查失败' : c.status}`, to: '/containers/' + c.id })
  for (const b of o.backups?.recentFailures || []) issues.push({ text: `备份失败：${b.label}（${ago(b.startedAt)}）`, to: '/backups' })
  const byProject = new Map<number, { name: string; services: string[] }>()
  for (const u of updates || []) {
    const e = byProject.get(u.projectId) || { name: u.project as string, services: [] as string[] }
    e.services.push(u.service)
    byProject.set(u.projectId, e)
  }
  for (const [pid, e] of byProject) issues.push({ text: `项目 ${e.name} 有镜像更新：${e.services.join('、')}`, to: `/projects/${pid}?tab=images`, kind: 'info' })
  for (const c of o.certs || []) if (c.daysLeft <= 14) issues.push({ text: `证书 ${c.domains?.[0]} 将在 ${c.daysLeft} 天后过期`, to: '/certificates' })

  return (
    <Page title="总览" sub={h.uptime ? `主机已运行 ${duration(h.uptime)}` : undefined}>
      {issues.length > 0 && (
        <Card title={`需要关注（${issues.length}）`} pad={false}>
          <ResponsiveTable
            table={
              <div className="table-wrap">
                <table>
                  <tbody>
                    {issues.slice(0, 8).map((i, k) => (
                      <tr key={k} className="clickable" onClick={() => nav(i.to)}>
                        <td>{i.kind === 'info' ? <span className="badge run"><span className="dot" />更新</span> : <span className="badge err"><span className="dot" />问题</span>}</td>
                        <td className="grow">{i.text}</td>
                        <td style={{ textAlign: 'right' }}><Link className="row-link" to={i.to} onClick={(e) => e.stopPropagation()}>查看</Link></td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            }
            cards={issues.slice(0, 8).map((i, k) => (
              <MobileCard
                key={k}
                tight
                onClick={() => nav(i.to)}
                title={i.text}
                badges={i.kind === 'info' ? <span className="badge run"><span className="dot" />更新</span> : <span className="badge err"><span className="dot" />问题</span>}
              />
            ))}
          />
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

      <MetricsPanel url="/metrics/host" host />

      <div className="dash-grid">
        <Card className="dash" title="项目" actions={<Link className="btn btn-sm" to="/projects">全部项目</Link>} pad={false}>
          {o.projects.length === 0 ? (
            <Empty icon="projects" action={<Link className="btn btn-primary" to="/projects/new">新建项目</Link>}>还没有项目。</Empty>
          ) : (
            <ResponsiveTable
              table={
                <div className="table-wrap">
                  <table>
                    <thead><tr><th>名称</th><th className="c-status">状态</th><th className="c-time">容器</th></tr></thead>
                    <tbody>
                      {o.projects.slice(0, ROWS).map((p: any) => (
                        <tr key={p.id} className="clickable" onClick={() => nav(`/projects/${p.id}`)}>
                          <td><div className="name"><Link className="row-link" to={`/projects/${p.id}`} onClick={(e) => e.stopPropagation()} title={p.name}>{p.name}</Link>{p.kind === 'database' && <span className="tag">数据库</span>}</div></td>
                          <td><Status s={p.status} /></td>
                          <td className="c-time mono">{p.running}/{p.total}</td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              }
              cards={o.projects.slice(0, ROWS).map((p: any) => (
                <MobileCard
                  key={p.id}
                  tight
                  onClick={() => nav(`/projects/${p.id}`)}
                  title={<><Link className="row-link" to={`/projects/${p.id}`} onClick={(e) => e.stopPropagation()}>{p.name}</Link>{p.kind === 'database' && <span className="tag">数据库</span>}</>}
                  sub={<span className="mono">容器 {p.running}/{p.total}</span>}
                  badges={<Status s={p.status} />}
                />
              ))}
            />
          )}
        </Card>

        <Card className="dash" title="最近部署" actions={<Link className="btn btn-sm" to="/jobs?kind=deploy">部署任务</Link>} pad={false}>
          {o.deployments.length === 0 ? (
            <Empty icon="rocket">还没有部署记录。</Empty>
          ) : (
            <ResponsiveTable
              table={
                <div className="table-wrap">
                  <table>
                    <thead><tr><th>项目</th><th className="c-status">状态</th><th className="c-trigger">触发</th><th className="c-time">时间</th></tr></thead>
                    <tbody>
                      {o.deployments.slice(0, ROWS).map((d: any) => (
                        <tr key={d.deployment.id} className="clickable" onClick={() => nav(`/deployments/${d.deployment.id}`)}>
                          <td>
                            <div className="name">
                              <Link className="row-link" to={`/deployments/${d.deployment.id}`} onClick={(e) => e.stopPropagation()} title={`${d.project} #${d.deployment.number}`}>{d.project}</Link>
                              <span className="dash-num">#{d.deployment.number}</span>
                            </div>
                          </td>
                          <td><JobStatus s={d.deployment.status} /></td>
                          <td className="c-trigger muted" title={`${triggerName(d.deployment.trigger)} · ${d.deployment.actor}`}>{triggerName(d.deployment.trigger)} · {d.deployment.actor}</td>
                          <td className="c-time muted">{ago(d.deployment.createdAt)}</td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              }
              cards={o.deployments.slice(0, ROWS).map((d: any) => (
                <MobileCard
                  key={d.deployment.id}
                  tight
                  onClick={() => nav(`/deployments/${d.deployment.id}`)}
                  title={<><Link className="row-link" to={`/deployments/${d.deployment.id}`} onClick={(e) => e.stopPropagation()}>{d.project}</Link><span className="dash-num">#{d.deployment.number}</span></>}
                  sub={joinInline([triggerName(d.deployment.trigger), d.deployment.actor, ago(d.deployment.createdAt)])}
                  badges={<JobStatus s={d.deployment.status} />}
                />
              ))}
            />
          )}
        </Card>

        <Card className="dash" title="最近任务" actions={<Link className="btn btn-sm" to="/jobs">任务中心</Link>} pad={false}>
          {o.jobs.length === 0 ? (
            <Empty icon="jobs">最近 6 小时没有任务。</Empty>
          ) : (
            <ResponsiveTable
              table={
                <div className="table-wrap">
                  <table>
                    <thead><tr><th>任务</th><th className="c-status">状态</th><th className="c-time">时间</th></tr></thead>
                    <tbody>
                      {o.jobs.slice(0, ROWS).map((j: any) => (
                        <tr key={j.id} className="clickable" onClick={() => openJob(j.id, j.title)}>
                          <td title={j.title}>{j.title}</td>
                          <td><JobStatus s={j.status} /></td>
                          <td className="c-time muted">{ago(j.createdAt)}</td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              }
              cards={o.jobs.slice(0, ROWS).map((j: any) => (
                <MobileCard
                  key={j.id}
                  tight
                  onClick={() => openJob(j.id, j.title)}
                  title={j.title}
                  sub={ago(j.createdAt)}
                  badges={<JobStatus s={j.status} />}
                />
              ))}
            />
          )}
        </Card>

        <Card className="dash" title="证书" actions={<Link className="btn btn-sm" to="/certificates">全部证书</Link>} pad={false}>
          {o.certs.length === 0 ? (
            <Empty icon="certs">近期没有即将过期的证书。</Empty>
          ) : (
            <ResponsiveTable
              table={
                <div className="table-wrap">
                  <table>
                    <thead><tr><th>域名</th><th className="c-time">剩余</th></tr></thead>
                    <tbody>
                      {o.certs.slice(0, ROWS).map((c: any) => (
                        <tr key={c.id + c.name}>
                          <td className="mono" title={c.domains?.join(', ')}>{c.domains?.join(', ')}</td>
                          <td className="c-time"><span className={'badge ' + (c.daysLeft <= 7 ? 'err' : 'warn')}>剩 {c.daysLeft} 天</span></td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              }
              cards={o.certs.slice(0, ROWS).map((c: any) => (
                <MobileCard
                  key={c.id + c.name}
                  tight
                  title={<span className="mono">{c.domains?.join(', ')}</span>}
                  badges={<span className={'badge ' + (c.daysLeft <= 7 ? 'err' : 'warn')}>剩 {c.daysLeft} 天</span>}
                />
              ))}
            />
          )}
        </Card>
      </div>
    </Page>
  )
}
