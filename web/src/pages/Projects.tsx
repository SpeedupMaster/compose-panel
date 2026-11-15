import { useState } from 'react'
import { Link, useNavigate } from 'react-router-dom'
import { ago, get } from '../api'
import { useCan } from '../auth'
import { Empty, ErrorBox, Icon, JobStatus, Loading, Page, Status, useLoad } from '../components/ui'

export default function Projects() {
  const { data, error } = useLoad(() => get<any[]>('/projects'), [], 10000)
  const can = useCan()
  const nav = useNavigate()
  const [q, setQ] = useState('')
  const [filter, setFilter] = useState('all')
  const list = (data || []).filter(
    (p) =>
      (filter === 'all' || (filter === 'db' ? p.kind === 'database' : filter === 'problem' ? ['error', 'partial'].includes(p.status) || p.unhealthy > 0 : p.kind !== 'database')) &&
      (!q || p.name.includes(q) || (p.domains || []).some((d: string) => d.includes(q))),
  )
  return (
    <Page
      title="项目"
      actions={
        can.operator && (
          <>
            {can.admin && <Link className="btn" to="/settings#import">导入已有项目</Link>}
            <Link className="btn btn-primary" to="/projects/new"><Icon name="plus" />新建项目</Link>
          </>
        )
      }
    >
      <div className="row wrap">
        <div className="row" style={{ position: 'relative', width: 280, maxWidth: '100%' }}>
          <input className="input" placeholder="搜索名称或域名" value={q} onChange={(e) => setQ(e.target.value)} aria-label="搜索项目" />
        </div>
        <div className="seg" role="group" aria-label="筛选">
          {[['all', '全部'], ['app', '应用'], ['db', '数据库'], ['problem', '有问题']].map(([k, l]) => (
            <button key={k} className={filter === k ? 'on' : ''} onClick={() => setFilter(k)}>{l}</button>
          ))}
        </div>
      </div>
      <ErrorBox error={error} />
      <section className="card">
        {!data ? (
          <Loading />
        ) : list.length === 0 ? (
          <Empty action={can.operator && data.length === 0 && <Link className="btn btn-primary" to="/projects/new">新建第一个项目</Link>}>
            {data.length === 0 ? '还没有项目。粘贴一个 compose.yml，或从 Git 仓库部署。' : '没有符合条件的项目。'}
          </Empty>
        ) : (
          <div className="table-wrap">
            <table>
              <thead>
                <tr><th>名称</th><th>状态</th><th>容器</th><th>域名</th><th>来源</th><th>最近部署</th></tr>
              </thead>
              <tbody>
                {list.map((p) => (
                  <tr key={p.id} className="clickable" onClick={() => nav(`/projects/${p.id}`)}>
                    <td>
                      <Link className="row-link" to={`/projects/${p.id}`} onClick={(e) => e.stopPropagation()}>{p.name}</Link>
                      {p.kind === 'database' && <span className="tag" style={{ marginLeft: 8 }}>数据库</span>}
                      {p.description && <div className="label ellipsis" style={{ maxWidth: 320 }}>{p.description}</div>}
                    </td>
                    <td><Status s={p.status} />{p.unhealthy > 0 && <span className="badge err" style={{ marginLeft: 6 }}>{p.unhealthy} 异常</span>}</td>
                    <td className="mono">{p.running}/{p.total}</td>
                    <td className="mono" style={{ fontSize: 13 }}>{(p.domains || []).slice(0, 2).join(', ') || <span className="muted">—</span>}{p.domains?.length > 2 && ` +${p.domains.length - 2}`}</td>
                    <td className="muted">{sourceName(p.source)}</td>
                    <td className="nowrap">
                      {p.lastDeploy ? (
                        <span className="row"><JobStatus s={p.lastDeploy.status} /><span className="muted">{ago(p.lastDeploy.createdAt)}</span></span>
                      ) : (
                        <span className="muted">—</span>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>
    </Page>
  )
}

export function sourceName(s: string) {
  return ({ inline: '面板编辑', git: 'Git 仓库', imported: '导入', database: '数据库' } as Record<string, string>)[s] || s
}
