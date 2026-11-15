import { useState } from 'react'
import { Link, useSearchParams } from 'react-router-dom'
import { ago, bytes, del, get, post } from '../api'
import { useCan } from '../auth'
import { Card, Confirm, Empty, ErrorBox, Field, Icon, Loading, Modal, Page, Status, Tabs, useLoad, useToast } from '../components/ui'
import { portsText } from './ProjectDetail'
import { FileBrowser } from '../components/files'

type Tab = 'containers' | 'images' | 'volumes' | 'networks' | 'registries'

export default function DockerResources() {
  const [params, setParams] = useSearchParams()
  const tab = (params.get('tab') as Tab) || 'containers'
  const can = useCan()
  const { data: df, reload: reloadDf } = useLoad(() => get<any>('/docker/df'), [])
  const [prune, setPrune] = useState<null | { kind: string; all: boolean; title: string; text: string }>(null)
  const toast = useToast()
  const tabs: [Tab, string][] = [['containers', '容器'], ['images', '镜像'], ['volumes', '数据卷'], ['networks', '网络']]
  if (can.operator) tabs.push(['registries', '镜像仓库凭据'])
  return (
    <Page title="Docker 资源">
      {df && (
        <div className="grid">
          <div className="card stat">
            <span className="label">镜像</span>
            <div className="big">{bytes(df.images)}</div>
            <span className="label">可回收 {bytes(df.imagesReclaimable)}</span>
            {can.admin && <div className="row"><button className="btn btn-sm" onClick={() => setPrune({ kind: 'images', all: false, title: '清理悬空镜像', text: '删除没有标签的悬空镜像。' })}>清理悬空</button><button className="btn btn-sm" onClick={() => setPrune({ kind: 'images', all: true, title: '清理未使用镜像', text: '删除所有未被容器使用的镜像，下次部署需要重新拉取。' })}>清理未使用</button></div>}
          </div>
          <div className="card stat">
            <span className="label">数据卷</span>
            <div className="big">{bytes(df.volumes)}</div>
            <span className="label">未被使用 {bytes(df.volumesReclaimable)}</span>
            {can.admin && <div className="row"><button className="btn btn-sm btn-danger" onClick={() => setPrune({ kind: 'volumes', all: true, title: '清理未使用数据卷', text: '删除所有未挂载到容器的数据卷，其中的数据将永久丢失。停止状态的项目的卷也可能被删除！' })}>清理未使用</button></div>}
          </div>
          <div className="card stat">
            <span className="label">构建缓存</span>
            <div className="big">{bytes(df.buildCache)}</div>
            {can.admin && <div className="row"><button className="btn btn-sm" onClick={() => setPrune({ kind: 'build', all: false, title: '清理构建缓存', text: '删除未使用的构建缓存。' })}>清理</button></div>}
          </div>
          <div className="card stat">
            <span className="label">容器</span>
            <div className="big">{df.containers}</div>
            {can.admin && <div className="row"><button className="btn btn-sm" onClick={() => setPrune({ kind: 'containers', all: false, title: '清理已停止容器', text: '删除所有已停止的容器。' })}>清理已停止</button></div>}
          </div>
        </div>
      )}
      <Tabs tabs={tabs} value={tab} onChange={(t) => setParams({ tab: t }, { replace: true })} />
      {tab === 'containers' && <Containers />}
      {tab === 'images' && <Images />}
      {tab === 'volumes' && <Volumes />}
      {tab === 'networks' && <Networks />}
      {tab === 'registries' && <Registries />}
      {prune && (
        <Confirm
          title={prune.title}
          danger={prune.kind === 'volumes'}
          typeToConfirm={prune.kind === 'volumes' ? 'prune' : undefined}
          confirmText="清理"
          onClose={() => setPrune(null)}
          onConfirm={async () => {
            const r = await post('/docker/prune', { kind: prune.kind, all: prune.all })
            toast.ok(`已清理，释放 ${bytes(r.spaceReclaimed)}`)
            reloadDf()
          }}
        >
          {prune.text}
        </Confirm>
      )}
    </Page>
  )
}

function Containers() {
  const { data, error } = useLoad(() => get<any[]>('/containers'), [], 10000)
  const [q, setQ] = useState('')
  if (!data) return error ? <ErrorBox error={error} /> : <Loading />
  const list = data.filter((c) => !q || c.name.includes(q) || c.image.includes(q) || c.project.includes(q))
  return (
    <section className="card">
      <div className="card-head"><input className="input" style={{ maxWidth: 280 }} placeholder="搜索容器、镜像、项目" aria-label="搜索容器" value={q} onChange={(e) => setQ(e.target.value)} /></div>
      <div className="table-wrap">
        <table>
          <thead><tr><th>名称</th><th>项目</th><th>状态</th><th>镜像</th><th>端口</th><th>创建</th></tr></thead>
          <tbody>
            {list.map((c) => (
              <tr key={c.id}>
                <td><Link to={`/containers/${c.id}`}>{c.name}</Link></td>
                <td>{c.project || <span className="muted">—</span>}</td>
                <td><Status s={c.health === 'unhealthy' ? 'unhealthy' : c.state} /><div className="label">{c.status}</div></td>
                <td className="mono ellipsis" style={{ maxWidth: 260, fontSize: 13 }} title={c.image}>{c.image}</td>
                <td className="mono" style={{ fontSize: 12 }}>{portsText(c.ports)}</td>
                <td className="muted nowrap">{ago(c.created)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </section>
  )
}

function Images() {
  const { data, error, reload } = useLoad(() => get<any[]>('/docker/images'), [])
  const can = useCan()
  const [rm, setRm] = useState<any>(null)
  if (!data) return error ? <ErrorBox error={error} /> : <Loading />
  return (
    <section className="card">
      <div className="table-wrap">
        <table>
          <thead><tr><th>标签</th><th>ID</th><th>大小</th><th>使用者</th><th>创建</th><th></th></tr></thead>
          <tbody>
            {data.map((im) => (
              <tr key={im.id}>
                <td className="mono" style={{ fontSize: 13 }}>{im.tags.length ? im.tags.join(', ') : <span className="muted">&lt;none&gt;</span>}</td>
                <td className="mono" style={{ fontSize: 12 }}>{im.id.replace('sha256:', '').slice(0, 12)}</td>
                <td className="nowrap">{bytes(im.size)}</td>
                <td>{im.usedBy.length ? im.usedBy.join(', ') : <span className="badge off">未使用</span>}</td>
                <td className="muted nowrap">{ago(im.created)}</td>
                <td>{can.admin && im.usedBy.length === 0 && <button className="btn btn-sm btn-ghost btn-icon" aria-label="删除镜像" onClick={() => setRm(im)}><Icon name="trash" size={16} /></button>}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {rm && <Confirm title="删除镜像" danger confirmText="删除" onClose={() => setRm(null)} onConfirm={async () => { await del(`/docker/images/${encodeURIComponent(rm.id)}`); reload() }}>删除 <code className="mono">{rm.tags[0] || rm.id.slice(7, 19)}</code>？</Confirm>}
    </section>
  )
}

function Volumes() {
  const { data, error, reload } = useLoad(() => get<any[]>('/docker/volumes'), [])
  const can = useCan()
  const [rm, setRm] = useState<any>(null)
  const [browse, setBrowse] = useState<string | null>(null)
  if (!data) return error ? <ErrorBox error={error} /> : <Loading />
  return (
    <section className="card">
      {browse && (
        <Modal title={<>数据卷 <span className="mono">{browse}</span></>} wide onClose={() => setBrowse(null)}>
          <FileBrowser base={`/docker/volumes/${encodeURIComponent(browse)}/fs`} canWrite note="通过临时 alpine 容器读写；上传的文件会继承目录的属主。" />
        </Modal>
      )}
      {data.length === 0 ? <Empty>没有数据卷。</Empty> : (
        <div className="table-wrap">
          <table>
            <thead><tr><th>名称</th><th>项目</th><th>大小</th><th>使用者</th><th></th></tr></thead>
            <tbody>
              {data.map((v) => (
                <tr key={v.name}>
                  <td className="mono" style={{ fontSize: 13 }}>{v.name}</td>
                  <td>{v.project || <span className="muted">—</span>}</td>
                  <td className="nowrap">{v.size >= 0 ? bytes(v.size) : '—'}</td>
                  <td>{v.usedBy.length ? v.usedBy.join(', ') : <span className="badge off">未使用</span>}</td>
                  <td>
                    <div className="row" style={{ justifyContent: 'flex-end' }}>
                      {can.admin && <button className="btn btn-sm" onClick={() => setBrowse(v.name)}><Icon name="folder" size={16} />文件</button>}
                      <Link className="btn btn-sm" to={`/backups?volume=${encodeURIComponent(v.name)}`}>备份</Link>
                      {can.admin && v.usedBy.length === 0 && <button className="btn btn-sm btn-ghost btn-icon" aria-label="删除数据卷" onClick={() => setRm(v)}><Icon name="trash" size={16} /></button>}
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {rm && <Confirm title="删除数据卷" danger confirmText="永久删除" typeToConfirm={rm.name} onClose={() => setRm(null)} onConfirm={async () => { await del(`/docker/volumes/${encodeURIComponent(rm.name)}`); reload() }}>卷中的数据将永久丢失，建议先备份。</Confirm>}
    </section>
  )
}

function Networks() {
  const { data, error, reload } = useLoad(() => get<any[]>('/docker/networks'), [])
  const can = useCan()
  const [rm, setRm] = useState<any>(null)
  if (!data) return error ? <ErrorBox error={error} /> : <Loading />
  return (
    <section className="card">
      <div className="table-wrap">
        <table>
          <thead><tr><th>名称</th><th>驱动</th><th>子网</th><th>容器</th><th></th></tr></thead>
          <tbody>
            {data.map((n) => (
              <tr key={n.id}>
                <td className="mono" style={{ fontSize: 13 }}>{n.name} {n.panel && <span className="badge run">面板</span>} {n.system && <span className="badge off">系统</span>} {n.internal && <span className="badge off">内部</span>}</td>
                <td>{n.driver}</td>
                <td className="mono" style={{ fontSize: 13 }}>{n.subnet || '—'}</td>
                <td>{n.containers?.length ? n.containers.join(', ') : <span className="muted">—</span>}</td>
                <td>{can.admin && !n.system && !n.panel && !n.containers?.length && <button className="btn btn-sm btn-ghost btn-icon" aria-label="删除网络" onClick={() => setRm(n)}><Icon name="trash" size={16} /></button>}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {rm && <Confirm title="删除网络" danger confirmText="删除" onClose={() => setRm(null)} onConfirm={async () => { await del(`/docker/networks/${rm.id}`); reload() }}>删除网络 <code className="mono">{rm.name}</code>？</Confirm>}
    </section>
  )
}

function Registries() {
  const { data, error, reload } = useLoad(() => get<any[]>('/registries'), [])
  const can = useCan()
  const toast = useToast()
  const [form, setForm] = useState<{ host: string; username: string; password: string } | null>(null)
  if (!data) return error ? <ErrorBox error={error} /> : <Loading />
  return (
    <Card title="镜像仓库凭据" actions={can.admin && <button className="btn btn-sm btn-primary" onClick={() => setForm({ host: '', username: '', password: '' })}><Icon name="plus" size={16} />添加</button>} pad={false}>
      <p className="label" style={{ padding: '12px 16px 0', margin: 0 }}>部署时用于拉取私有镜像（临时 DOCKER_CONFIG，不写入主机）。</p>
      {data.length === 0 ? <Empty>没有凭据。</Empty> : (
        <div className="table-wrap">
          <table>
            <thead><tr><th>地址</th><th>用户名</th><th></th></tr></thead>
            <tbody>
              {data.map((r) => (
                <tr key={r.id}>
                  <td className="mono">{r.host}</td>
                  <td>{r.username}</td>
                  <td>{can.admin && <button className="btn btn-sm btn-ghost btn-icon" aria-label="删除" onClick={async () => { await del(`/registries/${r.id}`).catch(toast.err); reload() }}><Icon name="trash" size={16} /></button>}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {form && (
        <Modal title="添加镜像仓库凭据" onClose={() => setForm(null)} footer={<><button className="btn" onClick={() => setForm(null)}>取消</button><button className="btn btn-primary" onClick={async () => { try { await post('/registries', form); setForm(null); reload() } catch (e) { toast.err(e) } }}>保存</button></>}>
          <Field label="地址" hint="如 ghcr.io、registry.example.com；Docker Hub 填 docker.io"><input className="input mono" value={form.host} onChange={(e) => setForm({ ...form, host: e.target.value })} /></Field>
          <Field label="用户名"><input className="input" value={form.username} onChange={(e) => setForm({ ...form, username: e.target.value })} /></Field>
          <Field label="密码 / 访问令牌"><input className="input" type="password" value={form.password} onChange={(e) => setForm({ ...form, password: e.target.value })} autoComplete="new-password" /></Field>
        </Modal>
      )}
    </Card>
  )
}
