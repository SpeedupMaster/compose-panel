import { useState } from 'react'
import { Link } from 'react-router-dom'
import { del, get, post, put } from '../api'
import { useCan } from '../auth'
import { Card, Confirm, Empty, ErrorBox, Field, Icon, Loading, Modal, Page, Select, useLoad, useToast } from '../components/ui'
import { AccessLogs } from './SiteLogs'
import { MobileCard, ResponsiveTable } from '../components/ResponsiveList'

type Site = {
  id?: number
  domains: string[]
  kind: 'proxy' | 'redirect'
  projectId: number
  service: string
  port: number
  upstream: string
  hsts: boolean
  compress: boolean
  basicUser: string
  basicPassword?: string
  hasBasicAuth?: boolean
  clearBasicAuth?: boolean
  ipAllow: string
  maxBody: string
  headers: string
  enabled: boolean
  maintenance: boolean
  maintenanceMsg: string
  maintenanceDeploy: boolean
  accessLog: boolean
  loadBalance: boolean
  healthPath: string
  grpc: boolean
}

export default function Sites() {
  const { data, error, reload } = useLoad(() => get<any>('/sites'), [], 20000)
  const can = useCan()
  const toast = useToast()
  const [edit, setEdit] = useState<Site | null>(null)
  const [rm, setRm] = useState<any>(null)
  const [cfg, setCfg] = useState<any>(null)
  const [logs, setLogs] = useState<any>(null)
  const [maint, setMaint] = useState<any>(null)
  const blank: Site = { domains: [], kind: 'proxy', projectId: 0, service: '', port: 80, upstream: '', hsts: true, compress: true, basicUser: '', ipAllow: '', maxBody: '', headers: '', enabled: true, maintenance: false, maintenanceMsg: '', maintenanceDeploy: false, accessLog: true, loadBalance: false, healthPath: '', grpc: false }
  return (
    <Page
      title="站点"
      sub="由内置 Caddy 反向代理，自动申请和续期 Let's Encrypt 证书"
      actions={
        <>
          {can.admin && <button className="btn" onClick={async () => { try { setCfg(await get('/proxy/config')) } catch (e) { toast.err(e) } }}>查看代理配置</button>}
          {can.admin && <button className="btn" onClick={async () => { try { await post('/proxy/reload'); toast.ok('已重新推送配置到 Caddy') } catch (e) { toast.err(e) } }}><Icon name="refresh" />重新加载</button>}
          {can.operator && <button className="btn btn-primary" onClick={() => setEdit(blank)}><Icon name="plus" />添加站点</button>}
        </>
      }
    >
      <ErrorBox error={error} />
      {data?.panelDomain && <div className="alert run">面板本身通过 <a href={'https://' + data.panelDomain} className="mono">{data.panelDomain}</a> 访问（在设置中修改）。</div>}
      <section className="card">
        {!data ? <Loading /> : data.sites.length === 0 ? (
          <Empty action={can.operator && <button className="btn btn-primary" onClick={() => setEdit(blank)}>添加站点</button>}>还没有站点。把域名解析到服务器后，在这里绑定到项目的服务。</Empty>
        ) : (
          <ResponsiveTable
            table={
              <div className="table-wrap">
                <table>
                  <thead><tr><th>域名</th><th>目标</th><th>证书</th><th>选项</th><th></th></tr></thead>
                  <tbody>
                    {data.sites.map(({ site: s, target, cert }: any) => (
                      <tr
                        key={s.id}
                        className={can.operator || s.kind !== 'redirect' ? 'clickable' : undefined}
                        onClick={() => {
                          if (can.operator) setEdit({ ...s, basicPassword: '' })
                          else if (s.kind !== 'redirect') setLogs(s)
                        }}
                      >
                        <td>
                          {s.domains.map((d: string) => <div key={d}><a className="sub-link mono" href={'https://' + d} target="_blank" rel="noreferrer" onClick={(e) => e.stopPropagation()}>{d}</a></div>)}
                          {!s.enabled && <span className="badge off">停用</span>}
                          {s.maintenance && <span className="badge warn"><span className="dot" />维护中</span>}
                        </td>
                        <td>
                          {s.kind === 'redirect' ? <span>跳转 → <span className="mono">{s.upstream}</span></span> : s.projectId ? <><Link className="sub-link" to={`/projects/${s.projectId}`} onClick={(e) => e.stopPropagation()}>{s.projectName}</Link> / <span className="mono">{s.service}:{s.port}</span></> : <span className="mono">{s.upstream}</span>}
                          <div className="label mono">{target}</div>
                        </td>
                        <td>{cert ? cert.source === 'internal' ? <span className="badge ok">本地 CA · 自动续期</span> : <span className={'badge ' + (cert.daysLeft <= 7 ? 'err' : cert.daysLeft <= 14 ? 'warn' : 'ok')}>{cert.source === 'uploaded' ? '上传' : "Let's Encrypt"} · 剩 {cert.daysLeft} 天</span> : <span className="badge off">待签发</span>}</td>
                        <td className="label">{[s.hsts && 'HSTS', s.compress && '压缩', s.hasBasicAuth && '密码保护', s.ipAllow && 'IP 白名单', s.maxBody && `上限 ${s.maxBody}`, s.loadBalance && '负载均衡', s.healthPath && `健康检查 ${s.healthPath}`, s.grpc && 'gRPC (h2c)', s.accessLog && '访问日志', s.maintenanceDeploy && '部署时维护'].filter(Boolean).join(' · ') || '—'}</td>
                        <td onClick={(e) => e.stopPropagation()}>
                          <div className="row" style={{ justifyContent: 'flex-end' }}>
                            {s.kind !== 'redirect' && <button className="btn btn-sm" onClick={() => setLogs(s)}><Icon name="logs" size={16} />日志</button>}
                            {can.operator && s.kind !== 'redirect' && <button className={'btn btn-sm' + (s.maintenance ? ' btn-primary' : '')} onClick={() => setMaint(s)}>{s.maintenance ? '结束维护' : '维护'}</button>}
                            {can.operator && <button className="btn btn-sm btn-ghost btn-icon" aria-label="编辑" onClick={() => setEdit({ ...s, basicPassword: '' })}><Icon name="edit" size={16} /></button>}
                            {can.operator && <button className="btn btn-sm btn-ghost btn-icon" aria-label="删除" onClick={() => setRm(s)}><Icon name="trash" size={16} /></button>}
                          </div>
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            }
            cards={data.sites.map(({ site: s, target, cert }: any) => {
              const opts = [s.hsts && 'HSTS', s.compress && '压缩', s.hasBasicAuth && '密码保护', s.ipAllow && 'IP 白名单', s.maxBody && `上限 ${s.maxBody}`, s.loadBalance && '负载均衡', s.healthPath && `健康检查 ${s.healthPath}`, s.grpc && 'gRPC (h2c)', s.accessLog && '访问日志', s.maintenanceDeploy && '部署时维护'].filter(Boolean).join(' · ')
              return (
                <MobileCard
                  key={s.id}
                  onClick={
                    can.operator || s.kind !== 'redirect'
                      ? () => {
                          if (can.operator) setEdit({ ...s, basicPassword: '' })
                          else if (s.kind !== 'redirect') setLogs(s)
                        }
                      : undefined
                  }
                  title={
                    <div className="col" style={{ gap: 2 }}>
                      {s.domains.map((d: string) => (
                        <a key={d} className="sub-link mono" href={'https://' + d} target="_blank" rel="noreferrer" onClick={(e) => e.stopPropagation()}>
                          {d}
                        </a>
                      ))}
                    </div>
                  }
                  sub={<span className="mono">{target}</span>}
                  badges={
                    <>
                      {!s.enabled && <span className="badge off">停用</span>}
                      {s.maintenance && <span className="badge warn"><span className="dot" />维护中</span>}
                      {cert ? (
                        cert.source === 'internal' ? (
                          <span className="badge ok">本地 CA · 自动续期</span>
                        ) : (
                          <span className={'badge ' + (cert.daysLeft <= 7 ? 'err' : cert.daysLeft <= 14 ? 'warn' : 'ok')}>
                            {cert.source === 'uploaded' ? '上传' : "Let's Encrypt"} · 剩 {cert.daysLeft} 天
                          </span>
                        )
                      ) : (
                        <span className="badge off">待签发</span>
                      )}
                    </>
                  }
                  meta={[
                    {
                      label: '目标',
                      value: s.kind === 'redirect' ? (
                        <span>跳转 → <span className="mono">{s.upstream}</span></span>
                      ) : s.projectId ? (
                        <>
                          <Link className="sub-link" to={`/projects/${s.projectId}`} onClick={(e) => e.stopPropagation()}>{s.projectName}</Link>
                          {' / '}
                          <span className="mono">{s.service}:{s.port}</span>
                        </>
                      ) : (
                        <span className="mono">{s.upstream}</span>
                      ),
                      full: true,
                    },
                    opts ? { label: '选项', value: opts, full: true } : false,
                  ]}
                  actions={
                    s.kind !== 'redirect' || can.operator ? (
                      <>
                        {s.kind !== 'redirect' && <button className="btn btn-sm" onClick={() => setLogs(s)}><Icon name="logs" size={16} />日志</button>}
                        {can.operator && s.kind !== 'redirect' && <button className={'btn btn-sm' + (s.maintenance ? ' btn-primary' : '')} onClick={() => setMaint(s)}>{s.maintenance ? '结束维护' : '维护'}</button>}
                        {can.operator && <button className="btn btn-sm btn-ghost btn-icon" aria-label="编辑" onClick={() => setEdit({ ...s, basicPassword: '' })}><Icon name="edit" size={16} /></button>}
                        {can.operator && <button className="btn btn-sm btn-ghost btn-icon" aria-label="删除" onClick={() => setRm(s)}><Icon name="trash" size={16} /></button>}
                      </>
                    ) : undefined
                  }
                />
              )
            })}
          />
        )}
      </section>
      {edit && <SiteModal s={edit} onClose={() => setEdit(null)} onSaved={() => { setEdit(null); reload() }} />}
      {logs && <AccessLogs site={logs} onClose={() => setLogs(null)} />}
      {maint && <MaintenanceModal s={maint} onClose={() => setMaint(null)} onDone={() => { setMaint(null); reload() }} />}
      {rm && <Confirm title="删除站点" danger confirmText="删除" onClose={() => setRm(null)} onConfirm={async () => { await del(`/sites/${rm.id}`); reload() }}>{rm.domains.join(', ')} 将不再被代理。</Confirm>}
      {cfg && (
        <Modal title="Caddy 配置（只读）" wide onClose={() => setCfg(null)}>
          <div>Caddy 状态：{cfg.caddy === 'ok' ? <span className="badge ok">正常</span> : <span className="badge err">{cfg.caddy}</span>}</div>
          <pre className="diff" style={{ margin: 0, padding: 12 }}>{JSON.stringify(cfg.config, null, 2)}</pre>
        </Modal>
      )}
    </Page>
  )
}

function SiteModal({ s, onClose, onSaved }: { s: Site; onClose: () => void; onSaved: () => void }) {
  const toast = useToast()
  const { data: projects } = useLoad(() => get<any[]>('/projects'), [])
  const [f, setF] = useState<Site>(s)
  const [domains, setDomains] = useState(s.domains.join('\n'))
  const [mode, setMode] = useState<'project' | 'upstream' | 'redirect'>(s.kind === 'redirect' ? 'redirect' : s.projectId || !s.id ? 'project' : 'upstream')
  const [check, setCheck] = useState<any>(null)
  const [err, setErr] = useState<unknown>()
  const [adv, setAdv] = useState(!!(s.ipAllow || s.maxBody || s.headers || s.basicUser))
  const { data: proj } = useLoad(() => (f.projectId ? get<any>(`/projects/${f.projectId}`) : Promise.resolve(null)), [f.projectId])
  const services = [...new Set<string>((proj?.containers || []).map((c: any) => c.service).filter(Boolean))]
  const submit = async () => {
    setErr(undefined)
    const body = { ...f, domains: domains.split(/[\s,]+/).filter(Boolean), kind: mode === 'redirect' ? 'redirect' : 'proxy', projectId: mode === 'project' ? f.projectId : 0, service: mode === 'project' ? f.service || services[0] || '' : '' }
    try {
      const r = s.id ? await put(`/sites/${s.id}`, body) : await post('/sites', body)
      if (r.warning) toast.err(r.warning)
      else toast.ok('站点已保存，证书会在首次访问前自动申请')
      onSaved()
    } catch (e) {
      setErr(e)
    }
  }
  const precheck = async () => {
    const d = domains.split(/[\s,]+/).filter(Boolean)[0]
    if (!d) return
    try {
      setCheck(await post('/sites/precheck', { domain: d }))
    } catch (e) {
      setCheck({ error: (e as Error).message })
    }
  }
  return (
    <Modal title={s.id ? '编辑站点' : '添加站点'} wide onClose={onClose} footer={<><button className="btn" onClick={onClose}>取消</button><button className="btn btn-primary" onClick={submit}>保存</button></>}>
      <Field label="域名" hint="每行一个；第一个域名用于检查 DNS">
        <textarea className="textarea mono" rows={2} value={domains} onChange={(e) => { setDomains(e.target.value); setCheck(null) }} placeholder="app.example.com" />
      </Field>
      <div className="row wrap">
        <button className="btn btn-sm" onClick={precheck}>检查 DNS 解析</button>
        {check && (check.error ? <span className="badge err">{check.error}</span> : (
          <span className="row wrap">
            <span className="mono" style={{ fontSize: 13, overflowWrap: 'anywhere' }}>{check.domain} → {check.ips.join(', ') || '无记录'}</span>
            {check.publicIp && (check.matches ? <span className="badge ok">指向本机</span> : <span className="badge warn">未指向 {check.publicIp}</span>)}
            {check.note && <span className="badge warn">{check.note}</span>}
          </span>
        ))}
      </div>
      <div className="seg" role="group" aria-label="目标类型">
        <button className={mode === 'project' ? 'on' : ''} onClick={() => setMode('project')}>项目服务</button>
        <button className={mode === 'upstream' ? 'on' : ''} onClick={() => setMode('upstream')}>自定义上游</button>
        <button className={mode === 'redirect' ? 'on' : ''} onClick={() => setMode('redirect')}>跳转</button>
      </div>
      {mode === 'project' && (
        <div className="form-grid">
          <Field label="项目">
            <Select value={f.projectId || ''} onChange={(v) => setF({ ...f, projectId: Number(v), service: '' })}>
              <option value="">选择项目</option>
              {(projects || []).filter((p) => p.kind !== 'database').map((p) => <option key={p.id} value={p.id}>{p.name}</option>)}
            </Select>
          </Field>
          <Field label="服务">
            {services.length ? (
              <Select value={f.service || services[0]} onChange={(v) => setF({ ...f, service: v })}>{services.map((x) => <option key={x}>{x}</option>)}</Select>
            ) : (
              <input className="input mono" value={f.service} onChange={(e) => setF({ ...f, service: e.target.value })} placeholder="web" />
            )}
          </Field>
          <Field label="容器端口"><input className="input mono" value={f.port || ''} onChange={(e) => setF({ ...f, port: Number(e.target.value) || 0 })} /></Field>
        </div>
      )}
      {mode === 'upstream' && <Field label="上游地址" hint="host:port，需与 Caddy 处于同一网络或可访问"><input className="input mono" value={f.upstream} onChange={(e) => setF({ ...f, upstream: e.target.value })} placeholder="container-name:8080" /></Field>}
      {mode === 'redirect' && <Field label="跳转到" hint="永久跳转（308），保留路径"><input className="input mono" value={f.upstream} onChange={(e) => setF({ ...f, upstream: e.target.value })} placeholder="https://www.example.com" /></Field>}
      <div className="row wrap">
        <label className="check"><input type="checkbox" checked={f.hsts} onChange={(e) => setF({ ...f, hsts: e.target.checked })} />HSTS</label>
        <label className="check"><input type="checkbox" checked={f.compress} onChange={(e) => setF({ ...f, compress: e.target.checked })} />gzip / zstd 压缩</label>
        <label className="check"><input type="checkbox" checked={f.enabled} onChange={(e) => setF({ ...f, enabled: e.target.checked })} />启用</label>
        <button className="btn btn-sm btn-ghost" onClick={() => setAdv(!adv)}>{adv ? '收起高级选项' : '高级选项'}</button>
      </div>
      {mode !== 'redirect' && (
        <Card title="可用性与日志">
          <div className="col" style={{ gap: 10 }}>
            <label className="check"><input type="checkbox" checked={f.grpc} onChange={(e) => setF({ ...f, grpc: e.target.checked })} />gRPC 上游（h2c）：gRPC 请求以 HTTP/2 明文转发给后端，其余请求（REST、SSE、WebSocket）不变</label>
            <label className="check"><input type="checkbox" checked={f.accessLog} onChange={(e) => setF({ ...f, accessLog: e.target.checked })} />记录访问日志（JSON，按 20MB 轮转，保留 7 天）</label>
            {mode === 'project' && (
              <>
                <label className="check"><input type="checkbox" checked={f.loadBalance} onChange={(e) => setF({ ...f, loadBalance: e.target.checked })} />负载均衡：把请求轮询分发到该服务的所有副本（compose 中 deploy.replicas 或 scale）</label>
                <label className="check"><input type="checkbox" checked={f.maintenanceDeploy} onChange={(e) => setF({ ...f, maintenanceDeploy: e.target.checked })} />部署期间自动显示维护页面</label>
              </>
            )}
            <Field label="健康检查路径（可选）" hint="如 /healthz。设置后 Caddy 每 10 秒主动探测，不健康的副本不再接收请求">
              <input className="input mono" style={{ maxWidth: 280 }} value={f.healthPath} onChange={(e) => setF({ ...f, healthPath: e.target.value })} placeholder="/healthz" />
            </Field>
            <Field label="维护页面提示（可选）" hint="手动维护或部署维护时显示给访客">
              <input className="input" value={f.maintenanceMsg} onChange={(e) => setF({ ...f, maintenanceMsg: e.target.value })} placeholder="系统维护中，请稍后再试" />
            </Field>
          </div>
        </Card>
      )}
      {adv && (
        <Card title="高级">
          <div className="form-grid">
            <Field label="访问用户名（Basic Auth）"><input className="input" value={f.basicUser} onChange={(e) => setF({ ...f, basicUser: e.target.value })} autoComplete="off" /></Field>
            <Field label="访问密码" hint={s.hasBasicAuth ? '留空保持原密码' : undefined}><input className="input" type="password" value={f.basicPassword || ''} onChange={(e) => setF({ ...f, basicPassword: e.target.value })} autoComplete="new-password" /></Field>
            <Field label="IP 白名单" hint="逗号分隔的 IP 或 CIDR，留空不限"><input className="input mono" value={f.ipAllow} onChange={(e) => setF({ ...f, ipAllow: e.target.value })} /></Field>
            <Field label="请求体上限" hint="如 50MB"><input className="input mono" value={f.maxBody} onChange={(e) => setF({ ...f, maxBody: e.target.value })} /></Field>
          </div>
          {s.hasBasicAuth && <label className="check" style={{ marginTop: 8 }}><input type="checkbox" checked={!!f.clearBasicAuth} onChange={(e) => setF({ ...f, clearBasicAuth: e.target.checked })} />移除密码保护</label>}
          <Field label="自定义响应头" hint="每行一个 Name: value">
            <textarea className="textarea mono" rows={3} value={f.headers} onChange={(e) => setF({ ...f, headers: e.target.value })} />
          </Field>
        </Card>
      )}
      <ErrorBox error={err} />
    </Modal>
  )
}

function MaintenanceModal({ s, onClose, onDone }: { s: any; onClose: () => void; onDone: () => void }) {
  const [msg, setMsg] = useState(s.maintenanceMsg || '')
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState<unknown>()
  const toast = useToast()
  const on = !s.maintenance
  const submit = async () => {
    setBusy(true)
    setErr(undefined)
    try {
      const r = await post(`/sites/${s.id}/maintenance`, { on, message: msg })
      if (r.warning) toast.err(r.warning)
      else toast.ok(on ? '维护模式已开启' : '维护模式已关闭，恢复正常访问')
      onDone()
    } catch (e) {
      setErr(e)
    } finally {
      setBusy(false)
    }
  }
  return (
    <Modal
      title={on ? '开启维护模式' : '结束维护模式'}
      onClose={onClose}
      footer={<><button className="btn" onClick={onClose}>取消</button><button className={on ? 'btn btn-danger solid' : 'btn btn-primary'} disabled={busy} onClick={submit}>{on ? '开启维护' : '恢复访问'}</button></>}
    >
      {on ? (
        <>
          <p style={{ margin: 0 }}><span className="mono">{s.domains.join(', ')}</span> 的所有请求将返回 503 维护页面，后端服务不受影响。</p>
          <Field label="提示信息"><textarea className="textarea" rows={3} value={msg} onChange={(e) => setMsg(e.target.value)} placeholder="系统维护中，请稍后再试" /></Field>
        </>
      ) : (
        <p style={{ margin: 0 }}>恢复 <span className="mono">{s.domains.join(', ')}</span> 的正常访问？</p>
      )}
      <ErrorBox error={err} />
    </Modal>
  )
}
