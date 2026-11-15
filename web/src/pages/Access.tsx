import { useState } from 'react'
import { ago, dateTime, del, get, post, put } from '../api'
import { roleName, useAuth, useCan } from '../auth'
import { Card, Confirm, Empty, ErrorBox, Field, Icon, Loading, Modal, Page, Pager, SecretBox, Select, Tabs, useLoad, useToast } from '../components/ui'
import { MobileCard, ResponsiveTable } from '../components/ResponsiveList'

export default function Access() {
  const can = useCan()
  const [tab, setTab] = useState<'users' | 'tokens' | 'audit'>(can.admin ? 'users' : 'tokens')
  const tabs: ['users' | 'tokens' | 'audit', string][] = can.admin ? [['users', '用户'], ['tokens', 'API 令牌'], ['audit', '审计日志']] : [['tokens', 'API 令牌']]
  return (
    <Page title="用户与审计">
      <Tabs tabs={tabs} value={tab} onChange={setTab} />
      {tab === 'users' && <Users />}
      {tab === 'tokens' && <Tokens />}
      {tab === 'audit' && <Audit />}
    </Page>
  )
}

type UserForm = { id?: number; username: string; email: string; role: string; disabled: boolean; projects: number[] | null }

function Users() {
  const { data, error, reload } = useLoad(() => get<any[]>('/users'), [])
  const { me } = useAuth()
  const toast = useToast()
  const [edit, setEdit] = useState<UserForm | null>(null)
  const [pw, setPw] = useState<{ user: string; password: string } | null>(null)
  const [rm, setRm] = useState<any>(null)
  const [reset, setReset] = useState<any>(null)
  if (!data) return error ? <ErrorBox error={error} /> : <Loading />
  return (
    <Card title="用户" actions={<button className="btn btn-sm btn-primary" onClick={() => setEdit({ username: '', email: '', role: 'operator', disabled: false, projects: null })}><Icon name="plus" size={16} />添加用户</button>} pad={false}>
      <ResponsiveTable
        table={
          <div className="table-wrap">
            <table>
              <thead><tr><th>用户</th><th>角色</th><th>项目范围</th><th>两步验证</th><th>最近登录</th><th></th></tr></thead>
              <tbody>
                {data.map((u) => (
                  <tr key={u.id}>
                    <td><span style={{ fontWeight: 600 }}>{u.username}</span>{u.disabled && <span className="badge off" style={{ marginLeft: 6 }}>已停用</span>}<div className="label">{u.email}</div></td>
                    <td>{roleName[u.role]}</td>
                    <td>{u.role === 'admin' || u.projects === null ? '全部项目' : `${u.projects.length} 个项目`}</td>
                    <td>{u.totp ? <span className="badge ok">已开启</span> : <span className="badge warn">未开启</span>}</td>
                    <td className="muted nowrap">{u.lastLoginAt ? `${ago(u.lastLoginAt)} · ${u.lastLoginIp}` : '从未'}</td>
                    <td>
                      <div className="row" style={{ justifyContent: 'flex-end' }}>
                        <button className="btn btn-sm" onClick={() => setReset(u)}>重置密码</button>
                        <button className="btn btn-sm btn-ghost btn-icon" aria-label="编辑" onClick={() => setEdit({ id: u.id, username: u.username, email: u.email, role: u.role, disabled: u.disabled, projects: u.projects })}><Icon name="edit" size={16} /></button>
                        {u.id !== me?.user.id && <button className="btn btn-sm btn-ghost btn-icon" aria-label="删除" onClick={() => setRm(u)}><Icon name="trash" size={16} /></button>}
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        }
        cards={data.map((u) => (
          <MobileCard
            key={u.id}
            title={u.username}
            sub={u.email}
            badges={
              <>
                {u.disabled && <span className="badge off">已停用</span>}
                {u.totp ? <span className="badge ok">已开启两步</span> : <span className="badge warn">未开启两步</span>}
              </>
            }
            meta={[
              { label: '角色', value: roleName[u.role] },
              { label: '项目范围', value: u.role === 'admin' || u.projects === null ? '全部项目' : `${u.projects.length} 个项目` },
              { label: '最近登录', value: u.lastLoginAt ? `${ago(u.lastLoginAt)} · ${u.lastLoginIp}` : '从未', full: true },
            ]}
            actions={
              <>
                <button className="btn btn-sm" onClick={() => setReset(u)}>重置密码</button>
                <button className="btn btn-sm btn-ghost btn-icon" aria-label="编辑" onClick={() => setEdit({ id: u.id, username: u.username, email: u.email, role: u.role, disabled: u.disabled, projects: u.projects })}><Icon name="edit" size={16} /></button>
                {u.id !== me?.user.id && <button className="btn btn-sm btn-ghost btn-icon" aria-label="删除" onClick={() => setRm(u)}><Icon name="trash" size={16} /></button>}
              </>
            }
          />
        ))}
      />
      {edit && <UserModal u={edit} onClose={() => setEdit(null)} onSaved={(r) => { setEdit(null); reload(); if (r?.password) setPw({ user: edit.username, password: r.password }) }} />}
      {pw && (
        <Modal title="初始密码" onClose={() => setPw(null)} footer={<button className="btn btn-primary" onClick={() => setPw(null)}>完成</button>}>
          <p style={{ margin: 0 }}>把下面的密码安全地发给 {pw.user}，只显示这一次，对方登录后应立即修改。</p>
          <SecretBox value={pw.password} />
        </Modal>
      )}
      {reset && <Confirm title={`重置 ${reset.username} 的密码`} confirmText="重置" onClose={() => setReset(null)} onConfirm={async () => { const r = await post(`/users/${reset.id}/reset-password`); setPw({ user: reset.username, password: r.password }) }}>将生成新的随机密码，并注销该用户的所有会话。</Confirm>}
      {rm && <Confirm title={`删除用户 ${rm.username}`} danger confirmText="删除" onClose={() => setRm(null)} onConfirm={async () => { await del(`/users/${rm.id}`).catch((e) => { toast.err(e); throw e }); reload() }}>该用户的会话和 API 令牌会一并删除。审计日志保留。</Confirm>}
    </Card>
  )
}

function UserModal({ u, onClose, onSaved }: { u: UserForm; onClose: () => void; onSaved: (r: any) => void }) {
  const { data: projects } = useLoad(() => get<any[]>('/projects'), [])
  const [f, setF] = useState<UserForm>(u)
  const [err, setErr] = useState<unknown>()
  const all = f.projects === null
  return (
    <Modal title={u.id ? '编辑用户' : '添加用户'} onClose={onClose} footer={<><button className="btn" onClick={onClose}>取消</button><button className="btn btn-primary" onClick={async () => { try { onSaved(u.id ? await put(`/users/${u.id}`, f) : await post('/users', f)) } catch (e) { setErr(e) } }}>保存</button></>}>
      <div className="form-grid">
        <Field label="用户名"><input className="input" value={f.username} readOnly={!!u.id} onChange={(e) => setF({ ...f, username: e.target.value })} /></Field>
        <Field label="邮箱"><input className="input" type="email" value={f.email} onChange={(e) => setF({ ...f, email: e.target.value })} /></Field>
      </div>
      <Field label="角色">
        <Select value={f.role} onChange={(v) => setF({ ...f, role: v })}>
          <option value="viewer">只读：查看状态与日志</option>
          <option value="operator">运维：部署、重启、备份、终端</option>
          <option value="admin">管理员：全部权限，包括用户与设置</option>
        </Select>
      </Field>
      {f.role !== 'admin' && (
        <Field label="可访问的项目">
          <div className="col" style={{ gap: 2 }}>
            <label className="check"><input type="checkbox" checked={all} onChange={(e) => setF({ ...f, projects: e.target.checked ? null : [] })} />全部项目（含以后新建的）</label>
            {!all && (
              <div className="col" style={{ gap: 2, maxHeight: 200, overflow: 'auto', paddingLeft: 24 }}>
                {(projects || []).map((p) => (
                  <label key={p.id} className="check"><input type="checkbox" checked={f.projects!.includes(p.id)} onChange={(e) => setF({ ...f, projects: e.target.checked ? [...f.projects!, p.id] : f.projects!.filter((x) => x !== p.id) })} />{p.name}</label>
                ))}
              </div>
            )}
          </div>
        </Field>
      )}
      {u.id && <label className="check"><input type="checkbox" checked={f.disabled} onChange={(e) => setF({ ...f, disabled: e.target.checked })} />停用此账号</label>}
      {!u.id && <p className="label" style={{ margin: 0 }}>保存后会生成随机初始密码。</p>}
      <ErrorBox error={err} />
    </Modal>
  )
}

function Tokens() {
  const { data, error, reload } = useLoad(() => get<any[]>('/tokens'), [])
  const can = useCan()
  const [create, setCreate] = useState<{ name: string; scope: string; days: number } | null>(null)
  const [tok, setTok] = useState<string | null>(null)
  const [rm, setRm] = useState<any>(null)
  const [err, setErr] = useState<unknown>()
  if (!data) return error ? <ErrorBox error={error} /> : <Loading />
  return (
    <Card title="API 令牌" actions={<button className="btn btn-sm btn-primary" onClick={() => setCreate({ name: '', scope: 'deploy', days: 90 })}><Icon name="plus" size={16} />创建令牌</button>} pad={false}>
      <p className="label" style={{ padding: '12px 16px 0', margin: 0 }}>用于 CI 等脚本调用：<code className="mono">curl -H "Authorization: Bearer cpt_…" -X POST {location.origin}/api/projects/ID/deploy</code></p>
      {data.length === 0 ? <Empty>没有令牌。</Empty> : (
        <ResponsiveTable
          table={
            <div className="table-wrap">
              <table>
                <thead><tr><th>名称</th><th>前缀</th><th>权限</th>{can.admin && <th>所有者</th>}<th>过期</th><th>最近使用</th><th></th></tr></thead>
                <tbody>
                  {data.map((t) => (
                    <tr key={t.id}>
                      <td style={{ fontWeight: 600 }}>{t.name}</td>
                      <td className="mono">{t.prefix}…</td>
                      <td>{({ read: '只读', deploy: '部署', full: '完全' } as Record<string, string>)[t.scope] || t.scope}</td>
                      {can.admin && <td>{t.owner}</td>}
                      <td className="muted nowrap">{t.expiresAt ? (t.expiresAt < Date.now() / 1000 ? <span className="badge err">已过期</span> : dateTime(t.expiresAt)) : '永不'}</td>
                      <td className="muted nowrap">{t.lastUsedAt ? `${ago(t.lastUsedAt)} · ${t.lastUsedIp}` : '从未'}</td>
                      <td><button className="btn btn-sm btn-ghost btn-icon" aria-label="吊销" onClick={() => setRm(t)}><Icon name="trash" size={16} /></button></td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          }
          cards={data.map((t) => (
            <MobileCard
              key={t.id}
              title={t.name}
              sub={<span className="mono">{t.prefix}…</span>}
              badges={
                <>
                  <span className="badge off">{({ read: '只读', deploy: '部署', full: '完全' } as Record<string, string>)[t.scope] || t.scope}</span>
                  {t.expiresAt && t.expiresAt < Date.now() / 1000 && <span className="badge err">已过期</span>}
                </>
              }
              meta={[
                can.admin ? { label: '所有者', value: t.owner } : false,
                { label: '过期', value: t.expiresAt ? dateTime(t.expiresAt) : '永不' },
                { label: '最近使用', value: t.lastUsedAt ? `${ago(t.lastUsedAt)} · ${t.lastUsedIp}` : '从未', full: true },
              ]}
              actions={<button className="btn btn-sm btn-danger" onClick={() => setRm(t)}><Icon name="trash" size={16} />吊销</button>}
            />
          ))}
        />
      )}
      {create && (
        <Modal title="创建 API 令牌" onClose={() => setCreate(null)} footer={<><button className="btn" onClick={() => setCreate(null)}>取消</button><button className="btn btn-primary" onClick={async () => { try { const r = await post('/tokens', create); setCreate(null); setTok(r.token); reload() } catch (e) { setErr(e) } }}>创建</button></>}>
          <Field label="名称"><input className="input" value={create.name} onChange={(e) => setCreate({ ...create, name: e.target.value })} placeholder="GitHub Actions" /></Field>
          <Field label="权限" hint="令牌权限不会超过你自己的角色">
            <Select value={create.scope} onChange={(v) => setCreate({ ...create, scope: v })}>
              <option value="read">只读</option>
              <option value="deploy">部署（读取 + 部署/重启）</option>
              <option value="full">完全（等同你的账号）</option>
            </Select>
          </Field>
          <Field label="有效期">
            <Select value={create.days} onChange={(v) => setCreate({ ...create, days: Number(v) })}>
              <option value={30}>30 天</option>
              <option value={90}>90 天</option>
              <option value={365}>1 年</option>
              <option value={0}>永不过期</option>
            </Select>
          </Field>
          <ErrorBox error={err} />
        </Modal>
      )}
      {tok && (
        <Modal title="令牌已创建" onClose={() => setTok(null)} footer={<button className="btn btn-primary" onClick={() => setTok(null)}>我已保存</button>}>
          <p style={{ margin: 0 }}>令牌只显示这一次。</p>
          <SecretBox value={tok} />
        </Modal>
      )}
      {rm && <Confirm title={`吊销令牌 ${rm.name}`} danger confirmText="吊销" onClose={() => setRm(null)} onConfirm={async () => { await del(`/tokens/${rm.id}`); reload() }}>使用该令牌的脚本将立即失效。</Confirm>}
    </Card>
  )
}

function Audit() {
  const [f, setF] = useState({ actor: '', action: '', q: '', result: '' })
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState(50)
  const qs = new URLSearchParams(Object.entries(f).filter(([, v]) => v)).toString()
  const { data, error } = useLoad(() => get<any>(`/audit?limit=${pageSize}&offset=${(page - 1) * pageSize}&${qs}`), [qs, page, pageSize])
  const toast = useToast()
  return (
    <section className="card">
      <div className="card-head">
        <div className="row wrap">
          <input className="input filter-input" style={{ width: 'min(140px, 100%)', flex: '1 1 120px' }} placeholder="用户" aria-label="用户" value={f.actor} onChange={(e) => { setF({ ...f, actor: e.target.value }); setPage(1) }} />
          <input className="input filter-input" style={{ width: 'min(160px, 100%)', flex: '1 1 140px' }} placeholder="操作，如 project." aria-label="操作" value={f.action} onChange={(e) => { setF({ ...f, action: e.target.value }); setPage(1) }} />
          <input className="input filter-input" style={{ width: 'min(180px, 100%)', flex: '1 1 160px' }} placeholder="搜索对象/详情" aria-label="搜索" value={f.q} onChange={(e) => { setF({ ...f, q: e.target.value }); setPage(1) }} />
          <Select className="filter-select" style={{ width: 110 }} aria-label="结果" value={f.result} onChange={(v) => { setF({ ...f, result: v }); setPage(1) }}>
            <option value="">全部</option>
            <option value="ok">成功</option>
            <option value="failed">失败</option>
          </Select>
        </div>
        <div className="row wrap">
          <button className="btn btn-sm" onClick={async () => { try { const r = await get('/audit/verify'); r.ok ? toast.ok(`哈希链完整（${r.count} 条）`) : toast.err(`哈希链在第 ${r.brokenAt} 条处断开，日志可能被篡改`) } catch (e) { toast.err(e) } }}><Icon name="check" size={16} />校验完整性</button>
          <a className="btn btn-sm" href={`/api/audit/export?${qs}`}><Icon name="download" size={16} />导出 CSV</a>
        </div>
      </div>
      <ErrorBox error={error} />
      {!data ? <Loading /> : (
        <>
          <div className="table-wrap">
            <table>
              <thead><tr><th>时间</th><th>用户</th><th>操作</th><th>对象</th><th>详情</th><th>IP</th><th>结果</th></tr></thead>
              <tbody>
                {data.items.map((a: any) => (
                  <tr key={a.id}>
                    <td className="muted nowrap" title={dateTime(a.ts)}>{dateTime(a.ts)}</td>
                    <td>{a.actor}</td>
                    <td className="mono" style={{ fontSize: 12 }}>{a.action}</td>
                    <td className="mono" style={{ fontSize: 12 }}>{a.target}</td>
                    <td className="ellipsis" style={{ maxWidth: 320 }} title={a.detail}>{a.detail}</td>
                    <td className="mono" style={{ fontSize: 12 }}>{a.ip}</td>
                    <td>{a.result === 'ok' ? <span className="badge ok">成功</span> : <span className="badge err" title={a.result}>失败</span>}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <Pager page={page} pageSize={pageSize} total={data.total} onPage={setPage} onPageSize={(n) => { setPageSize(n); setPage(1) }} sizes={[50, 100, 200]} />
        </>
      )}
    </section>
  )
}
