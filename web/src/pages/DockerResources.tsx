import { ReactNode, useState } from 'react'
import { Link, useNavigate, useSearchParams } from 'react-router-dom'
import { ago, bytes, del, get, post } from '../api'
import { useCan } from '../auth'
import { Card, Combobox, ComboOption, Confirm, Empty, ErrorBox, Field, Icon, Loading, Modal, Page, Status, Tabs, useLoad, useToast } from '../components/ui'
import { portsText } from '../components/shared'
import { FileBrowser } from '../components/files'
import { MobileCard, ResponsiveTable } from '../components/ResponsiveList'

type Tab = 'containers' | 'images' | 'volumes' | 'networks' | 'registries'

// Disk-usage card: total, the reclaimable share as a bar, and prune actions pinned to the bottom
// so every card in the row lines up.
function DfCard({ icon, title, count, value, part, total, partText, danger, calm, actions }: {
  icon: string; title: string; count: string; value: ReactNode; part: number; total: number; partText: string
  danger?: boolean; calm?: boolean; actions?: ReactNode
}) {
  const pct = total > 0 ? Math.min(100, Math.round((part / total) * 100)) : 0
  return (
    <div className="card df-card">
      <div className="df-head">
        <span className="df-icon"><Icon name={icon} size={16} /></span>
        <span className="df-title">{title}</span>
        <span className="df-count">{count}</span>
      </div>
      <div className="big">{value}</div>
      <div className="df-meter">
        <div className="df-meter-text"><span>{partText}</span>{total > 0 && <span>{pct}%</span>}</div>
        <div className={'track' + (danger ? ' err' : calm ? ' calm' : ' warn')}><div style={{ width: pct + '%' }} /></div>
      </div>
      {actions && <div className="df-actions">{actions}</div>}
    </div>
  )
}

export default function DockerResources() {
  const [params, setParams] = useSearchParams()
  const tab = (params.get('tab') as Tab) || 'containers'
  const can = useCan()
  const { data: df, reload: reloadDf } = useLoad(() => get<any>('/docker/df'), [])
  const [prune, setPrune] = useState<null | { kind: string; all: boolean; title: string; text: string }>(null)
  const toast = useToast()
  const stopped = df ? df.containers - (df.containersRunning ?? 0) : 0
  const tabs: [Tab, string][] = [['containers', '容器'], ['images', '镜像'], ['volumes', '数据卷'], ['networks', '网络']]
  if (can.operator) tabs.push(['registries', '镜像仓库凭据'])
  return (
    <Page title="Docker 资源">
      {df && (
        <div className="grid">
          <DfCard icon="image" title="镜像" count={`${df.imageCount ?? 0} 个`} value={bytes(df.images)}
            part={df.imagesReclaimable} total={df.images} partText={`可回收 ${bytes(df.imagesReclaimable)}`}
            actions={can.admin && <>
              <button className="btn btn-sm" onClick={() => setPrune({ kind: 'images', all: false, title: '清理悬空镜像', text: '删除没有标签的悬空镜像。' })}>清理悬空</button>
              <button className="btn btn-sm" onClick={() => setPrune({ kind: 'images', all: true, title: '清理未使用镜像', text: '删除所有未被容器使用的镜像，下次部署需要重新拉取。' })}>清理未使用</button>
            </>} />
          <DfCard icon="volume" title="数据卷" count={`${df.volumeCount ?? 0} 个`} value={bytes(df.volumes)}
            part={df.volumesReclaimable} total={df.volumes} partText={`未被使用 ${bytes(df.volumesReclaimable)}`} danger
            actions={can.admin && <button className="btn btn-sm btn-danger" onClick={() => setPrune({ kind: 'volumes', all: true, title: '清理未使用数据卷', text: '删除所有未挂载到容器的数据卷，其中的数据将永久丢失。停止状态的项目的卷也可能被删除！' })}>清理未使用</button>} />
          <DfCard icon="build" title="构建缓存" count={`${df.buildCacheCount ?? 0} 条`} value={bytes(df.buildCache)}
            part={df.buildCacheReclaimable ?? df.buildCache} total={df.buildCache} partText={`可回收 ${bytes(df.buildCacheReclaimable ?? df.buildCache)}`}
            actions={can.admin && <button className="btn btn-sm" onClick={() => setPrune({ kind: 'build', all: false, title: '清理构建缓存', text: '删除未使用的构建缓存。' })}>清理缓存</button>} />
          <DfCard icon="docker" title="容器" count={`${df.containers} 个`} value={<>{df.containersRunning ?? 0}<small> / {df.containers} 运行中</small></>}
            part={stopped} total={df.containers} calm
            partText={`已停止 ${stopped} 个${df.containersReclaimable ? ` · 占用 ${bytes(df.containersReclaimable)}` : ''}`}
            actions={can.admin && <button className="btn btn-sm" disabled={stopped === 0} onClick={() => setPrune({ kind: 'containers', all: false, title: '清理已停止容器', text: '删除所有已停止的容器。' })}>清理已停止</button>} />
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
  const nav = useNavigate()
  const [q, setQ] = useState('')
  if (!data) return error ? <ErrorBox error={error} /> : <Loading />
  const list = data.filter((c) => !q || c.name.includes(q) || c.image.includes(q) || c.project.includes(q))
  return (
    <section className="card">
      <div className="card-head"><input className="input filter-input" placeholder="搜索容器、镜像、项目" aria-label="搜索容器" value={q} onChange={(e) => setQ(e.target.value)} /></div>
      <ResponsiveTable
        table={
          <div className="table-wrap">
            <table>
              <thead><tr><th>名称</th><th>项目</th><th>状态</th><th>镜像</th><th>端口</th><th>创建</th></tr></thead>
              <tbody>
                {list.map((c) => (
                  <tr key={c.id} className="clickable" onClick={() => nav(`/containers/${c.id}`)}>
                    <td><Link className="row-link" to={`/containers/${c.id}`} onClick={(e) => e.stopPropagation()}>{c.name}</Link></td>
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
        }
        cards={list.map((c) => {
          const ports = portsText(c.ports)
          return (
            <MobileCard
              key={c.id}
              onClick={() => nav(`/containers/${c.id}`)}
              title={<Link className="row-link" to={`/containers/${c.id}`} onClick={(e) => e.stopPropagation()}>{c.name}</Link>}
              sub={c.project || '未关联项目'}
              badges={<Status s={c.health === 'unhealthy' ? 'unhealthy' : c.state} />}
              meta={[
                { label: '镜像', value: c.image, mono: true, full: true },
                { label: '运行态', value: c.status },
                { label: '创建', value: ago(c.created) },
                ports !== '—' ? { label: '端口', value: ports, mono: true, full: true } : false,
              ]}
            />
          )
        })}
      />
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
      <ResponsiveTable
        table={
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
        }
        cards={data.map((im) => (
          <MobileCard
            key={im.id}
            title={<span className="mono">{im.tags.length ? im.tags.join(', ') : '<none>'}</span>}
            sub={<span className="mono">{im.id.replace('sha256:', '').slice(0, 12)}</span>}
            badges={im.usedBy.length === 0 ? <span className="badge off">未使用</span> : undefined}
            meta={[
              { label: '大小', value: bytes(im.size) },
              { label: '创建', value: ago(im.created) },
              im.usedBy.length ? { label: '使用者', value: im.usedBy.join(', '), full: true } : false,
            ]}
            actions={can.admin && im.usedBy.length === 0 ? <button className="btn btn-sm btn-danger" onClick={() => setRm(im)}><Icon name="trash" size={16} />删除镜像</button> : undefined}
          />
        ))}
      />
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
        <ResponsiveTable
          table={
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
          }
          cards={data.map((v) => (
            <MobileCard
              key={v.name}
              title={<span className="mono">{v.name}</span>}
              sub={v.project || undefined}
              badges={v.usedBy.length === 0 ? <span className="badge off">未使用</span> : undefined}
              meta={[
                { label: '大小', value: v.size >= 0 ? bytes(v.size) : '—' },
                v.usedBy.length ? { label: '使用者', value: v.usedBy.join(', '), full: true } : false,
              ]}
              actions={
                <>
                  {can.admin && <button className="btn btn-sm" onClick={() => setBrowse(v.name)}><Icon name="folder" size={16} />文件</button>}
                  <Link className="btn btn-sm" to={`/backups?volume=${encodeURIComponent(v.name)}`}>备份</Link>
                  {can.admin && v.usedBy.length === 0 && <button className="btn btn-sm btn-ghost btn-icon" aria-label="删除数据卷" onClick={() => setRm(v)}><Icon name="trash" size={16} /></button>}
                </>
              }
            />
          ))}
        />
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
      <ResponsiveTable
        table={
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
        }
        cards={data.map((n) => (
          <MobileCard
            key={n.id}
            title={<span className="mono">{n.name}</span>}
            badges={
              <>
                {n.panel && <span className="badge run">面板</span>}
                {n.system && <span className="badge off">系统</span>}
                {n.internal && <span className="badge off">内部</span>}
              </>
            }
            meta={[
              { label: '驱动', value: n.driver },
              { label: '子网', value: n.subnet || '—', mono: true },
              n.containers?.length ? { label: '容器', value: n.containers.join(', '), full: true } : false,
            ]}
            actions={can.admin && !n.system && !n.panel && !n.containers?.length ? <button className="btn btn-sm btn-danger" onClick={() => setRm(n)}><Icon name="trash" size={16} />删除网络</button> : undefined}
          />
        ))}
      />
      {rm && <Confirm title="删除网络" danger confirmText="删除" onClose={() => setRm(null)} onConfirm={async () => { await del(`/docker/networks/${rm.id}`); reload() }}>删除网络 <code className="mono">{rm.name}</code>？</Confirm>}
    </section>
  )
}

// Common registries offered in the address field; users can still type any host.
const knownRegistries: [string, string, string?][] = [
  ['docker.io', 'Docker Hub'],
  ['ghcr.io', 'GitHub Container Registry'],
  ['registry.gitlab.com', 'GitLab Container Registry'],
  ['quay.io', 'Red Hat Quay'],
  ['public.ecr.aws', 'Amazon ECR Public'],
  ['us-docker.pkg.dev', 'Google Artifact Registry', '按区域替换'],
  ['registry.cn-hangzhou.aliyuncs.com', '阿里云 ACR', '按地域替换'],
  ['ccr.ccs.tencentyun.com', '腾讯云 TCR 个人版'],
  ['swr.cn-north-4.myhuaweicloud.com', '华为云 SWR', '按区域替换'],
]

// Registry host of an image reference, or '' for Docker Hub short names (nginx, me/app).
function registryHost(ref: string) {
  const first = ref.split('/')[0]
  return ref.includes('/') && (first.includes('.') || first.includes(':') || first === 'localhost') ? first : ''
}

function registryOptions(images: any[] | undefined, existing: string[]): ComboOption[] {
  const taken = new Set(existing)
  const known = new Set(knownRegistries.map(([h]) => h))
  const local = [...new Set((images || []).flatMap((im) => (im.tags || []).map(registryHost)).filter((h: string) => h && !known.has(h) && h !== 'docker.io'))].sort()
  const tag = (h: string) => (taken.has(h) ? '已添加' : undefined)
  return [
    ...local.map((h) => ({ value: h, label: '本机镜像中使用', hint: tag(h) })),
    ...knownRegistries.map(([h, l, note]) => ({ value: h, label: note ? `${l} · ${note}` : l, hint: tag(h) })),
  ]
}

function Registries() {
  const { data, error, reload } = useLoad(() => get<any[]>('/registries'), [])
  const { data: images } = useLoad(() => get<any[]>('/docker/images').catch(() => []), [])
  const can = useCan()
  const toast = useToast()
  const [form, setForm] = useState<{ host: string; username: string; password: string } | null>(null)
  if (!data) return error ? <ErrorBox error={error} /> : <Loading />
  return (
    <Card title="镜像仓库凭据" actions={can.admin && <button className="btn btn-sm btn-primary" onClick={() => setForm({ host: '', username: '', password: '' })}><Icon name="plus" size={16} />添加</button>} pad={false}>
      <p className="label" style={{ padding: '12px 16px 0', margin: 0 }}>部署时用于拉取私有镜像（临时 DOCKER_CONFIG，不写入主机）。</p>
      {data.length === 0 ? <Empty>没有凭据。</Empty> : (
        <ResponsiveTable
          table={
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
          }
          cards={data.map((r) => (
            <MobileCard
              key={r.id}
              tight
              title={<span className="mono">{r.host}</span>}
              sub={`用户名：${r.username}`}
              badges={can.admin ? <button className="btn btn-sm btn-ghost btn-icon" aria-label="删除" onClick={async () => { await del(`/registries/${r.id}`).catch(toast.err); reload() }}><Icon name="trash" size={16} /></button> : undefined}
            />
          ))}
        />
      )}
      {form && (
        <Modal title="添加镜像仓库凭据" onClose={() => setForm(null)} footer={<><button className="btn" onClick={() => setForm(null)}>取消</button><button className="btn btn-primary" onClick={async () => { try { await post('/registries', form); setForm(null); reload() } catch (e) { toast.err(e) } }}>保存</button></>}>
          <Field div label="地址" hint="从列表选择，或直接输入仓库地址，如 registry.example.com:5000">
            <Combobox mono autoFocus aria-label="仓库地址" value={form.host} placeholder="docker.io" onChange={(host) => setForm({ ...form, host })}
              options={registryOptions(images, data.map((r) => r.host))} />
          </Field>
          <Field label="用户名"><input className="input" value={form.username} onChange={(e) => setForm({ ...form, username: e.target.value })} /></Field>
          <Field label="密码 / 访问令牌"><input className="input" type="password" value={form.password} onChange={(e) => setForm({ ...form, password: e.target.value })} autoComplete="new-password" /></Field>
        </Modal>
      )}
    </Card>
  )
}
