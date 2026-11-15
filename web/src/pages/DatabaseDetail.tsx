import { useState } from 'react'
import { Link, useNavigate, useParams, useSearchParams } from 'react-router-dom'
import { bytes, del, get, post, put } from '../api'
import { useCan } from '../auth'
import { BackupNowModal, BackupTable } from '../components/backup'
import { Card, Confirm, Empty, ErrorBox, Field, Icon, Loading, Modal, Page, SecretBox, Select, Status, Tabs, useJobLog, useLoad, useToast } from '../components/ui'
import { defaultImage, engineLabel, exposureName, ImageField } from './Databases'
import { ImportExport, SlowQueries, SqlConsole, TableBrowser } from './DatabaseTools'

type Tab = 'overview' | 'dbs' | 'users' | 'sessions' | 'sql' | 'tables' | 'slow' | 'io' | 'backups' | 'settings'

export default function DatabaseDetail() {
  const { id } = useParams()
  const [params, setParams] = useSearchParams()
  const tab = (params.get('tab') as Tab) || 'overview'
  const { data, error, reload } = useLoad(() => get<any>(`/databases/${id}`), [id], 15000)
  if (!data) return <Page title="数据库" crumbs={<Link to="/databases">数据库</Link>}>{error ? <ErrorBox error={error} /> : <Loading />}</Page>
  const d = data.instance
  const sql = data.engine?.sql
  const isRedis = d.engine === 'redis'
  const tabs: [Tab, string][] = [['overview', '概览']]
  if (!isRedis) tabs.push(['dbs', '库'], ['users', '用户'])
  if (sql) tabs.push(['tables', '表浏览'], ['sql', 'SQL 控制台'], ['sessions', '会话'], ['slow', '慢查询'])
  if (!isRedis) tabs.push(['io', '导入导出'])
  tabs.push(['backups', '备份'], ['settings', '设置'])
  return (
    <Page
      title={d.name}
      crumbs={<><Link to="/databases">数据库</Link> / {d.name}</>}
      sub={<span className="row wrap"><Status s={d.status} /><span>{engineLabel(d.engine)} {d.version}</span>{d.image && <span className="mono" title="自定义镜像">· {d.image}</span>}<span>· {exposureName[d.exposure]}</span></span>}
      actions={<Link className="btn" to={`/projects/${d.projectId}`}>容器与日志</Link>}
    >
      <Tabs tabs={tabs} value={tab} onChange={(t) => setParams(t === 'overview' ? {} : { tab: t }, { replace: true })} />
      {d.status !== 'running' && tab !== 'overview' && tab !== 'backups' && tab !== 'settings' && <div className="alert warn">实例未运行，无法读取。</div>}
      {tab === 'overview' && <OverviewTab data={data} />}
      {tab === 'dbs' && d.status === 'running' && <DbsTab id={id!} />}
      {tab === 'users' && d.status === 'running' && <UsersTab id={id!} />}
      {tab === 'sessions' && d.status === 'running' && <SessionsTab id={id!} />}
      {tab === 'sql' && d.status === 'running' && <SqlConsole id={id!} instance={d.name} />}
      {tab === 'tables' && d.status === 'running' && <TableBrowser id={id!} />}
      {tab === 'slow' && d.status === 'running' && <SlowQueries id={id!} engine={d.engine} />}
      {tab === 'io' && d.status === 'running' && <ImportExport id={id!} engine={d.engine} sql={!!sql} />}
      {tab === 'backups' && <BackupsTab d={d} />}
      {tab === 'settings' && <SettingsTab data={data} onSaved={reload} />}
    </Page>
  )
}

const metricName: Record<string, string> = { connections: '连接数', maxConnections: '最大连接', cacheHit: '缓存命中率', longestTx: '最长事务', version: '服务端版本', memory: '内存', keys: '键数量', clients: '客户端', uptime: '运行时间', ops: '每秒操作' }

function OverviewTab({ data }: { data: any }) {
  const d = data.instance
  const c = data.connection
  const can = useCan()
  const [cred, setCred] = useState<any>(null)
  const toast = useToast()
  const [iu, ip] = data.initCredential ? [data.initCredential.slice(0, data.initCredential.indexOf(':')), data.initCredential.slice(data.initCredential.indexOf(':') + 1)] : ['', '']
  return (
    <>
      {Object.keys(data.metrics || {}).length > 0 && (
        <div className="grid">
          {Object.entries(data.metrics).map(([k, v]) => (
            <div key={k} className="card stat"><span className="label">{metricName[k] || k}</span><div className="big" style={{ fontSize: 20, overflowWrap: 'anywhere' }} title={String(v)}>{k === 'version' ? String(v).split(/[\s,]+/).slice(0, 2).join(' ') : String(v)}</div></div>
          ))}
        </div>
      )}
      <div className="grid-2">
        <Card title="连接信息">
          <dl className="kv">
            <dt>内部主机名</dt><dd className="mono">{c.host}:{c.port}</dd>
            <dt>所在网络</dt><dd className="mono">{c.network}</dd>
            {c.hostUrl && <><dt>主机访问</dt><dd className="mono">{c.hostUrl}</dd></>}
            <dt>连接串</dt><dd className="mono" style={{ fontSize: 12 }}>{c.internalUrl}</dd>
          </dl>
          <p className="label">应用容器需加入外部网络 <code className="mono">{c.network}</code> 才能访问（在 compose 里声明 networks: [{c.network}] 并设为 external）。</p>
        </Card>
        <Card title="凭据">
          <div className="col" style={{ gap: 12 }}>
            {cred ? (
              <>
                <SecretBox label="管理员用户" value={cred.user} />
                <SecretBox label="管理员密码" value={cred.password} />
              </>
            ) : can.operator ? (
              <div><button className="btn" onClick={async () => { try { setCred(await get(`/databases/${d.id}/credentials`)) } catch (e) { toast.err(e) } }}><Icon name="key" />显示管理员密码</button><div className="label" style={{ marginTop: 6 }}>查看会记录到审计日志。</div></div>
            ) : (
              <span className="muted">只读用户无法查看凭据。</span>
            )}
            {iu && (
              <>
                <SecretBox label="初始应用用户" value={iu} />
                <SecretBox label="初始应用用户密码" value={ip} />
              </>
            )}
          </div>
        </Card>
      </div>
    </>
  )
}

function PasswordShown({ r, onClose }: { r: { user: string; password: string } | null; onClose: () => void }) {
  if (!r) return null
  return (
    <Modal title="请保存密码" onClose={onClose} footer={<button className="btn btn-primary" onClick={onClose}>我已保存</button>}>
      <p style={{ margin: 0 }}>密码只显示这一次。</p>
      <SecretBox label="用户" value={r.user} />
      <SecretBox label="密码" value={r.password} />
    </Modal>
  )
}

function DbsTab({ id }: { id: string }) {
  const { data, error, reload } = useLoad(() => get<any[]>(`/databases/${id}/dbs`), [id])
  const can = useCan()
  const toast = useToast()
  const [creating, setCreating] = useState<{ name: string; user: string } | null>(null)
  const [shown, setShown] = useState<any>(null)
  const [drop, setDrop] = useState<string | null>(null)
  if (!data) return error ? <ErrorBox error={error} /> : <Loading />
  return (
    <Card title="库" actions={can.operator && <button className="btn btn-sm btn-primary" onClick={() => setCreating({ name: '', user: '' })}><Icon name="plus" size={16} />新建库</button>} pad={false}>
      {data.length === 0 ? <Empty>没有库。</Empty> : (
        <div className="table-wrap">
          <table>
            <thead><tr><th>名称</th><th>所有者</th><th>大小</th><th></th></tr></thead>
            <tbody>
              {data.map((x) => (
                <tr key={x.name}>
                  <td className="mono">{x.name}</td>
                  <td>{x.owner || '—'}</td>
                  <td>{x.size ? bytes(x.size) : '—'}</td>
                  <td><div className="row" style={{ justifyContent: 'flex-end' }}>{can.admin && <button className="btn btn-sm btn-ghost btn-icon" aria-label={'删除库 ' + x.name} onClick={() => setDrop(x.name)}><Icon name="trash" size={16} /></button>}</div></td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {creating && (
        <Modal title="新建库" onClose={() => setCreating(null)} footer={<><button className="btn" onClick={() => setCreating(null)}>取消</button><button className="btn btn-primary" onClick={async () => { try { const r = await post(`/databases/${id}/dbs`, creating); setCreating(null); reload(); if (r.password) setShown(r) } catch (e) { toast.err(e) } }}>创建</button></>}>
          <Field label="库名"><input className="input mono" value={creating.name} onChange={(e) => setCreating({ ...creating, name: e.target.value })} autoFocus /></Field>
          <Field label="同时创建专属用户（可选）" hint="生成随机密码，只拥有该库的权限"><input className="input mono" value={creating.user} onChange={(e) => setCreating({ ...creating, user: e.target.value })} /></Field>
        </Modal>
      )}
      <PasswordShown r={shown} onClose={() => setShown(null)} />
      {drop && <Confirm title={`删除库 ${drop}`} danger confirmText="删除" typeToConfirm={drop} onClose={() => setDrop(null)} onConfirm={async () => { await del(`/databases/${id}/dbs/${drop}`); reload() }}>库中的全部数据将被删除。建议先备份。</Confirm>}
    </Card>
  )
}

function UsersTab({ id }: { id: string }) {
  const { data, error, reload } = useLoad(() => get<any[]>(`/databases/${id}/users`), [id])
  const { data: dbs } = useLoad(() => get<any[]>(`/databases/${id}/dbs`), [id])
  const can = useCan()
  const toast = useToast()
  const [creating, setCreating] = useState<{ name: string; databases: string[]; readOnly: boolean } | null>(null)
  const [shown, setShown] = useState<any>(null)
  const [drop, setDrop] = useState<string | null>(null)
  if (!data) return error ? <ErrorBox error={error} /> : <Loading />
  return (
    <Card title="用户" actions={can.operator && <button className="btn btn-sm btn-primary" onClick={() => setCreating({ name: '', databases: [], readOnly: false })}><Icon name="plus" size={16} />新建用户</button>} pad={false}>
      <div className="table-wrap">
        <table>
          <thead><tr><th>用户</th><th>权限</th><th>可访问库</th><th></th></tr></thead>
          <tbody>
            {data.map((u) => (
              <tr key={u.name + (u.host || '')}>
                <td className="mono">{u.name}{u.host && <span className="muted">@{u.host}</span>}</td>
                <td>{u.superuser ? <span className="badge warn">超级用户</span> : <span className="badge off">普通</span>}</td>
                <td className="mono" style={{ fontSize: 13 }}>{u.databases || '—'}</td>
                <td>
                  <div className="row" style={{ justifyContent: 'flex-end' }}>
                    {can.operator && !u.superuser && <button className="btn btn-sm" onClick={async () => { try { setShown(await post(`/databases/${id}/users/${encodeURIComponent(u.name)}/password`)) } catch (e) { toast.err(e) } }}>重置密码</button>}
                    {can.admin && !u.superuser && <button className="btn btn-sm btn-ghost btn-icon" aria-label={'删除用户 ' + u.name} onClick={() => setDrop(u.name)}><Icon name="trash" size={16} /></button>}
                  </div>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {creating && (
        <Modal title="新建用户" onClose={() => setCreating(null)} footer={<><button className="btn" onClick={() => setCreating(null)}>取消</button><button className="btn btn-primary" onClick={async () => { try { const r = await post(`/databases/${id}/users`, creating); setCreating(null); reload(); setShown(r) } catch (e) { toast.err(e) } }}>创建</button></>}>
          <Field label="用户名"><input className="input mono" value={creating.name} onChange={(e) => setCreating({ ...creating, name: e.target.value })} autoFocus /></Field>
          <Field label="可访问的库">
            <div className="col" style={{ gap: 2, maxHeight: 200, overflow: 'auto' }}>
              {(dbs || []).map((x) => (
                <label key={x.name} className="check"><input type="checkbox" checked={creating.databases.includes(x.name)} onChange={(e) => setCreating({ ...creating, databases: e.target.checked ? [...creating.databases, x.name] : creating.databases.filter((n) => n !== x.name) })} /><span className="mono">{x.name}</span></label>
              ))}
            </div>
          </Field>
          <label className="check"><input type="checkbox" checked={creating.readOnly} onChange={(e) => setCreating({ ...creating, readOnly: e.target.checked })} />只读权限</label>
        </Modal>
      )}
      <PasswordShown r={shown} onClose={() => setShown(null)} />
      {drop && <Confirm title={`删除用户 ${drop}`} danger confirmText="删除" onClose={() => setDrop(null)} onConfirm={async () => { await del(`/databases/${id}/users/${encodeURIComponent(drop)}`); reload() }}>使用该用户的应用将无法连接。</Confirm>}
    </Card>
  )
}

function SessionsTab({ id }: { id: string }) {
  const { data, error, reload } = useLoad(() => get<any[]>(`/databases/${id}/sessions`), [id], 5000)
  const can = useCan()
  const toast = useToast()
  if (!data) return error ? <ErrorBox error={error} /> : <Loading />
  return (
    <Card title={`会话（${data.length}）`} pad={false}>
      <div className="table-wrap">
        <table>
          <thead><tr><th>PID</th><th>用户</th><th>库</th><th>客户端</th><th>状态</th><th>耗时</th><th>语句</th><th></th></tr></thead>
          <tbody>
            {data.map((s) => (
              <tr key={s.pid}>
                <td className="mono">{s.pid}</td>
                <td>{s.user}</td>
                <td className="mono">{s.database}</td>
                <td className="mono" style={{ fontSize: 12 }}>{s.client}</td>
                <td>{s.state}</td>
                <td className="mono">{s.seconds}s</td>
                <td className="mono ellipsis" style={{ maxWidth: 360, fontSize: 12 }} title={s.query}>{s.query}</td>
                <td>{can.operator && <button className="btn btn-sm" onClick={async () => { try { await del(`/databases/${id}/sessions/${s.pid}`); reload() } catch (e) { toast.err(e) } }}>终止</button>}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </Card>
  )
}

function BackupsTab({ d }: { d: any }) {
  const { data, reload } = useLoad(() => get<any[]>(`/backups?instance=${d.id}`), [d.id], 10000)
  const { data: policies } = useLoad(() => get<any[]>('/backup-policies'), [])
  const can = useCan()
  const [now, setNow] = useState<string | null>(null)
  const [bkDb, setBkDb] = useState('')
  const { data: dbs } = useLoad(() => (d.status === 'running' && d.engine !== 'redis' ? get<any[]>(`/databases/${d.id}/dbs`) : Promise.resolve([{ name: 'all' }])), [d.id])
  const mine = (policies || []).filter((p) => p.source.kind === 'database' && p.source.instanceId === d.id)
  return (
    <>
      <Card title="备份策略" actions={<Link className="btn btn-sm" to={`/backups?new=database&instance=${d.id}`}>新建策略</Link>} pad={false}>
        {mine.length === 0 ? <Empty>这个实例还没有定时备份。</Empty> : (
          <div className="table-wrap"><table><tbody>
            {mine.map((p) => <tr key={p.id}><td>{p.name}</td><td className="mono">{p.source.database}</td><td className="mono">{p.schedule}</td><td>{p.storageName}</td><td>{p.enabled ? <span className="badge ok">启用</span> : <span className="badge off">停用</span>}</td></tr>)}
          </tbody></table></div>
        )}
      </Card>
      <Card
        title="备份记录"
        actions={can.operator && (dbs || []).length > 0 && (
          <div className="row">
            <Select className="sm" style={{ width: 160 }} aria-label="选择库" value={bkDb || dbs?.[0]?.name} onChange={setBkDb}>
              {(dbs || []).map((x) => <option key={x.name}>{x.name}</option>)}
            </Select>
            <button className="btn btn-sm btn-primary" onClick={() => setNow(bkDb || dbs?.[0]?.name)}>立即备份</button>
          </div>
        )}
        pad={false}
      >
        {data ? <BackupTable list={data} reload={reload} names={{ db: { [d.id]: d.name } }} /> : <Loading />}
      </Card>
      {now && <BackupNowModal source={{ kind: 'database', instanceId: d.id, database: now }} onClose={() => setNow(null)} onDone={reload} />}
    </>
  )
}

function SettingsTab({ data, onSaved }: { data: any; onSaved: () => void }) {
  const d = data.instance
  const can = useCan()
  const toast = useToast()
  const nav = useNavigate()
  const openJob = useJobLog()
  const [f, setF] = useState({ version: d.version, exposure: d.exposure, port: d.port, memLimit: '', image: d.image || '' })
  const pg = d.engine === 'postgres'
  // PostgreSQL can't change major version in place; its versions are all majors.
  const versions: string[] = pg ? [d.version] : [...new Set<string>([d.version, ...(data.engine?.versions || [])])]
  const [deleting, setDeleting] = useState(false)
  return (
    <>
      <Card title="实例配置">
        <div className="form-grid">
          <Field label="版本" hint={pg ? 'PostgreSQL 大版本不能原地升级：新建目标版本实例，再用备份导出/导入迁移' : '只建议升级小版本；大版本升级需要导出导入'}>
            <Select value={f.version} disabled={!can.operator || versions.length < 2} onChange={(v) => setF({ ...f, version: v })}>
              {versions.map((v) => <option key={v}>{v}</option>)}
            </Select>
          </Field>
          <Field label="访问方式">
            <Select value={f.exposure} disabled={!can.operator} onChange={(v) => setF({ ...f, exposure: v })}>
              <option value="internal">仅内部网络</option>
              <option value="local">本机 127.0.0.1</option>
              <option value="public">公网</option>
            </Select>
          </Field>
          <Field label="端口"><input className="input mono" value={f.port} readOnly={!can.operator} onChange={(e) => setF({ ...f, port: Number(e.target.value) || 0 })} /></Field>
          <Field label="内存上限" hint="留空保持不变"><input className="input mono" value={f.memLimit} readOnly={!can.operator} placeholder="512m" onChange={(e) => setF({ ...f, memLimit: e.target.value })} /></Field>
        </div>
        <div style={{ marginTop: 14 }}>
          <ImageField engine={d.engine} value={f.image} version={f.version} placeholder={defaultImage(data.engine, f.version)} readOnly={!can.operator} onChange={(image) => setF({ ...f, image })} />
        </div>
        {can.operator && (
          <div className="row" style={{ justifyContent: 'flex-end', marginTop: 16 }}>
            <button className="btn btn-primary" onClick={async () => { try { const r = await put(`/databases/${d.id}`, f); openJob(r.jobId, `更新 ${d.name}`); onSaved() } catch (e) { toast.err(e) } }}>保存并重建容器</button>
          </div>
        )}
      </Card>
      {can.admin && (
        <Card title="危险操作">
          <div className="row wrap">
            <div className="grow"><div style={{ fontWeight: 600 }}>删除实例</div><div className="label">删除容器与数据卷。已有的备份文件会保留。</div></div>
            <button className="btn btn-danger" onClick={() => setDeleting(true)}>删除实例…</button>
          </div>
        </Card>
      )}
      {deleting && (
        <Confirm title={`删除数据库实例 ${d.name}`} danger confirmText="永久删除" typeToConfirm={d.name} onClose={() => setDeleting(false)} onConfirm={async () => { const r = await del(`/databases/${d.id}?confirm=${encodeURIComponent(d.name)}`); openJob(r.jobId, `删除 ${d.name}`); nav('/databases') }}>
          所有数据将被永久删除。请确认已有可用的备份。
        </Confirm>
      )}
    </>
  )
}
