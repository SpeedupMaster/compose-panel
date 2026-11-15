import { useState } from 'react'
import { Link, useNavigate } from 'react-router-dom'
import { ago, bytes, get, post } from '../api'
import { useCan } from '../auth'
import { Empty, ErrorBox, Field, Icon, Loading, Modal, Page, Select, Status, useJobLog, useLoad } from '../components/ui'

export const exposureName: Record<string, string> = { internal: '仅内部网络', local: '本机 127.0.0.1', public: '公网' }

export default function Databases() {
  const { data, error } = useLoad(() => get<any[]>('/databases'), [], 10000)
  const can = useCan()
  const nav = useNavigate()
  const [creating, setCreating] = useState(false)
  return (
    <Page title="数据库" actions={can.operator && <button className="btn btn-primary" onClick={() => setCreating(true)}><Icon name="plus" />新建数据库</button>}>
      <ErrorBox error={error} />
      <section className="card">
        {!data ? <Loading /> : data.length === 0 ? (
          <Empty action={can.operator && <button className="btn btn-primary" onClick={() => setCreating(true)}>新建数据库</button>}>
            还没有数据库实例。面板可以创建 PostgreSQL、MySQL、MariaDB、MongoDB 和 Redis，并自动配置备份。
          </Empty>
        ) : (
          <div className="table-wrap">
            <table>
              <thead><tr><th>名称</th><th>引擎</th><th>状态</th><th>访问</th><th>数据量</th><th>最近备份</th></tr></thead>
              <tbody>
                {data.map(({ instance: d, lastBackupAt, lastBackupStatus, volumeSize }) => (
                  <tr key={d.id} className="clickable" onClick={() => nav(`/databases/${d.id}`)}>
                    <td><Link to={`/databases/${d.id}`} onClick={(e) => e.stopPropagation()} style={{ fontWeight: 600 }}>{d.name}</Link><div className="label mono">{d.host}:{d.port}</div></td>
                    <td>{engineLabel(d.engine)} <span className="muted">{d.version}</span></td>
                    <td><Status s={d.status} /></td>
                    <td>{exposureName[d.exposure] || d.exposure}</td>
                    <td className="nowrap">{volumeSize > 0 ? bytes(volumeSize) : '—'}</td>
                    <td className="nowrap">{lastBackupAt ? <span className="row"><Status s={lastBackupStatus} /><span className="muted">{ago(lastBackupAt)}</span></span> : <span className="badge warn">从未备份</span>}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>
      {creating && <CreateDB onClose={() => setCreating(false)} />}
    </Page>
  )
}

export function engineLabel(id: string) {
  return ({ postgres: 'PostgreSQL', mysql: 'MySQL', mariadb: 'MariaDB', mongo: 'MongoDB', redis: 'Redis' } as Record<string, string>)[id] || id
}

function CreateDB({ onClose }: { onClose: () => void }) {
  const { data: engines } = useLoad(() => get<any[]>('/db-engines'))
  const nav = useNavigate()
  const openJob = useJobLog()
  const [f, setF] = useState({ name: '', engine: 'postgres', version: '', exposure: 'internal', port: 0, memLimit: '512m', initDb: '', initUser: '' })
  const [err, setErr] = useState<unknown>()
  const [busy, setBusy] = useState(false)
  const eng = engines?.find((e) => e.id === f.engine)
  const submit = async () => {
    setBusy(true)
    setErr(undefined)
    try {
      const r = await post('/databases', { ...f, version: f.version || eng?.versions[0], port: f.port || eng?.port })
      openJob(r.jobId, `创建数据库 ${f.name}`)
      nav(`/databases/${r.id}`)
    } catch (e) {
      setErr(e)
    } finally {
      setBusy(false)
    }
  }
  return (
    <Modal title="新建数据库" onClose={onClose} footer={<><button className="btn" onClick={onClose}>取消</button><button className="btn btn-primary" disabled={busy || !f.name} onClick={submit}>{busy ? '创建中…' : '创建'}</button></>}>
      <div className="grid" style={{ gridTemplateColumns: 'repeat(auto-fit, minmax(110px, 1fr))', gap: 8 }}>
        {(engines || []).map((e) => (
          <button key={e.id} className={'btn' + (f.engine === e.id ? ' btn-primary' : '')} style={{ height: 44 }} onClick={() => setF({ ...f, engine: e.id, version: '', port: 0 })}>{e.name}</button>
        ))}
      </div>
      <div className="form-grid">
        <Field label="实例名" hint="也是内部网络中的主机名"><input className="input mono" value={f.name} onChange={(e) => setF({ ...f, name: e.target.value.toLowerCase() })} placeholder={f.engine + '-main'} autoFocus /></Field>
        <Field label="版本">
          <Select value={f.version || eng?.versions[0] || ''} onChange={(v) => setF({ ...f, version: v })}>
            {(eng?.versions || []).map((v: string) => <option key={v}>{v}</option>)}
          </Select>
        </Field>
        <Field label="访问方式">
          <Select value={f.exposure} onChange={(v) => setF({ ...f, exposure: v })}>
            <option value="internal">仅内部网络（推荐）</option>
            <option value="local">映射到本机 127.0.0.1</option>
            <option value="public">映射到公网（不推荐）</option>
          </Select>
        </Field>
        <Field label="端口"><input className="input mono" inputMode="numeric" value={f.port || eng?.port || ''} onChange={(e) => setF({ ...f, port: Number(e.target.value) || 0 })} /></Field>
        <Field label="内存上限"><input className="input mono" value={f.memLimit} onChange={(e) => setF({ ...f, memLimit: e.target.value })} /></Field>
      </div>
      {f.exposure === 'public' && <div className="alert warn">公网暴露的数据库容易被扫描和爆破，请确认防火墙和强密码。</div>}
      {f.engine !== 'redis' && (
        <div className="form-grid">
          <Field label="初始库名（可选）"><input className="input mono" value={f.initDb} onChange={(e) => setF({ ...f, initDb: e.target.value })} placeholder="app" /></Field>
          <Field label="初始用户（可选）" hint="会生成随机密码，并只授予该库的权限"><input className="input mono" value={f.initUser} onChange={(e) => setF({ ...f, initUser: e.target.value })} placeholder="app" /></Field>
        </div>
      )}
      <p className="label" style={{ margin: 0 }}>root 密码随机生成并加密保存。应用项目可通过共享网络 <code className="mono">panel-db</code> 以实例名访问。</p>
      <ErrorBox error={err} />
    </Modal>
  )
}
