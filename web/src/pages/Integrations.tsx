import { useEffect, useState } from 'react'
import { ago, bytes, del, get, post, put } from '../api'
import { Card, Confirm, Empty, ErrorBox, Field, Icon, Loading, Modal, Page, SecretBox, Tabs, useLoad, useToast } from '../components/ui'

export default function Integrations() {
  const [tab, setTab] = useState<'storage' | 'notify' | 'key'>('storage')
  return (
    <Page title="存储与通知">
      <Tabs tabs={[['storage', '备份存储'], ['notify', '通知'], ['key', '备份加密密钥']]} value={tab} onChange={setTab} />
      {tab === 'storage' && <Storage />}
      {tab === 'notify' && <Notify />}
      {tab === 'key' && <BackupKey />}
    </Page>
  )
}

type Target = { id?: number; name: string; kind: 'local' | 's3'; config: any }

function Storage() {
  const { data, error, reload } = useLoad(() => get<any[]>('/storage'), [])
  const toast = useToast()
  const [edit, setEdit] = useState<Target | null>(null)
  const [rm, setRm] = useState<any>(null)
  if (!data) return error ? <ErrorBox error={error} /> : <Loading />
  return (
    <Card title="备份存储位置" actions={<button className="btn btn-sm btn-primary" onClick={() => setEdit({ name: '', kind: 's3', config: { region: 'auto', prefix: '' } })}><Icon name="plus" size={16} />添加</button>} pad={false}>
      <div className="table-wrap">
        <table>
          <thead><tr><th>名称</th><th>类型</th><th>位置</th><th>备份</th><th>连接测试</th><th></th></tr></thead>
          <tbody>
            {data.map(({ target: t, backups, size, policies }) => (
              <tr key={t.id}>
                <td style={{ fontWeight: 600 }}>{t.name}</td>
                <td>{t.kind === 's3' ? 'S3 兼容' : '本地磁盘'}</td>
                <td className="mono" style={{ fontSize: 12 }}>{t.kind === 's3' ? `${t.config.endpoint}/${t.config.bucket}/${t.config.prefix || ''}` : t.config.path}</td>
                <td>{backups} 份 · {bytes(size)}<div className="label">{policies} 个策略</div></td>
                <td>{t.lastCheckAt ? <span className={'badge ' + (t.lastCheckOk ? 'ok' : 'err')} title={t.lastCheckMsg}>{t.lastCheckOk ? '正常' : '失败'} · {ago(t.lastCheckAt)}</span> : <span className="muted">未测试</span>}</td>
                <td>
                  <div className="row" style={{ justifyContent: 'flex-end' }}>
                    <button className="btn btn-sm" onClick={async () => { try { const r = await post(`/storage/${t.id}/test`); r.ok ? toast.ok(`连接正常（${r.elapsedMs} ms）`) : toast.err(r.message); reload() } catch (e) { toast.err(e) } }}>测试</button>
                    <button className="btn btn-sm btn-ghost btn-icon" aria-label="编辑" onClick={() => setEdit({ id: t.id, name: t.name, kind: t.kind, config: { ...t.config } })}><Icon name="edit" size={16} /></button>
                    <button className="btn btn-sm btn-ghost btn-icon" aria-label="删除" onClick={() => setRm(t)}><Icon name="trash" size={16} /></button>
                  </div>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {edit && <StorageModal t={edit} onClose={() => setEdit(null)} onSaved={() => { setEdit(null); reload() }} />}
      {rm && <Confirm title={`删除存储 ${rm.name}`} danger confirmText="删除" onClose={() => setRm(null)} onConfirm={async () => { await del(`/storage/${rm.id}`); reload() }}>只删除面板中的配置，存储中的备份文件不会被删除。仍被策略使用的存储无法删除。</Confirm>}
    </Card>
  )
}

function StorageModal({ t, onClose, onSaved }: { t: Target; onClose: () => void; onSaved: () => void }) {
  const [f, setF] = useState<Target>(t)
  const [err, setErr] = useState<unknown>()
  const [test, setTest] = useState<any>(null)
  const c = f.config
  const setC = (patch: any) => setF({ ...f, config: { ...c, ...patch } })
  return (
    <Modal
      title={t.id ? '编辑存储' : '添加存储'}
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={async () => { try { setTest(await post(t.id ? `/storage/${t.id}/test` : '/storage/test', f)) } catch (e) { setTest({ ok: false, message: (e as Error).message }) } }}>测试连接</button>
          <span className="spacer" />
          <button className="btn" onClick={onClose}>取消</button>
          <button className="btn btn-primary" onClick={async () => { try { t.id ? await put(`/storage/${t.id}`, f) : await post('/storage', f); onSaved() } catch (e) { setErr(e) } }}>保存</button>
        </>
      }
    >
      <Field label="名称"><input className="input" value={f.name} onChange={(e) => setF({ ...f, name: e.target.value })} /></Field>
      <div className="seg" role="group" aria-label="类型">
        <button className={f.kind === 's3' ? 'on' : ''} onClick={() => setF({ ...f, kind: 's3' })}>S3 兼容（R2 / OSS / COS / MinIO / B2）</button>
        <button className={f.kind === 'local' ? 'on' : ''} onClick={() => setF({ ...f, kind: 'local' })}>本地目录</button>
      </div>
      {f.kind === 'local' ? (
        <Field label="目录" hint="面板容器内的路径；默认备份目录已挂载在 /data/backups"><input className="input mono" value={c.path || ''} onChange={(e) => setC({ path: e.target.value })} /></Field>
      ) : (
        <div className="form-grid">
          <Field label="Endpoint" hint="不含协议，如 xxx.r2.cloudflarestorage.com"><input className="input mono" value={c.endpoint || ''} onChange={(e) => setC({ endpoint: e.target.value })} /></Field>
          <Field label="Region"><input className="input mono" value={c.region || ''} onChange={(e) => setC({ region: e.target.value })} /></Field>
          <Field label="Bucket"><input className="input mono" value={c.bucket || ''} onChange={(e) => setC({ bucket: e.target.value })} /></Field>
          <Field label="前缀（可选）"><input className="input mono" value={c.prefix || ''} onChange={(e) => setC({ prefix: e.target.value })} placeholder="panel/" /></Field>
          <Field label="Access Key"><input className="input mono" value={c.accessKey || ''} onChange={(e) => setC({ accessKey: e.target.value })} autoComplete="off" /></Field>
          <Field label="Secret Key" hint={t.id ? '留空保持不变' : undefined}><input className="input mono" type="password" value={c.secretKey === '••••••' ? '' : c.secretKey || ''} onChange={(e) => setC({ secretKey: e.target.value })} autoComplete="new-password" /></Field>
          <label className="check"><input type="checkbox" checked={!!c.pathStyle} onChange={(e) => setC({ pathStyle: e.target.checked })} />Path-style 访问（MinIO 常用）</label>
          <label className="check"><input type="checkbox" checked={!!c.insecure} onChange={(e) => setC({ insecure: e.target.checked })} />使用 HTTP（不加密）</label>
        </div>
      )}
      {test && <div className={'alert ' + (test.ok ? 'ok' : 'err')}>{test.ok ? `连接正常（${test.elapsedMs} ms）` : test.message}</div>}
      <ErrorBox error={err} />
    </Modal>
  )
}

type Channel = { id?: number; name: string; kind: 'webhook' | 'telegram' | 'wecom' | 'email'; config: any; enabled: boolean }
const kindName: Record<string, string> = { webhook: 'Webhook', telegram: 'Telegram', wecom: '企业微信机器人', email: '邮件 (SMTP)' }

function Notify() {
  const { data, error, reload } = useLoad(() => get<any>('/notify'), [])
  const toast = useToast()
  const [edit, setEdit] = useState<Channel | null>(null)
  const [rm, setRm] = useState<any>(null)
  const [rules, setRules] = useState<Record<string, number[]>>({})
  const [dirty, setDirty] = useState(false)
  useEffect(() => {
    if (data) {
      setRules(data.rules || {})
      setDirty(false)
    }
  }, [data])
  if (!data) return error ? <ErrorBox error={error} /> : <Loading />
  const toggle = (ev: string, cid: number) => {
    const cur = rules[ev] || []
    setRules({ ...rules, [ev]: cur.includes(cid) ? cur.filter((x) => x !== cid) : [...cur, cid] })
    setDirty(true)
  }
  return (
    <>
      <Card title="通知渠道" actions={<button className="btn btn-sm btn-primary" onClick={() => setEdit({ name: '', kind: 'telegram', config: {}, enabled: true })}><Icon name="plus" size={16} />添加渠道</button>} pad={false}>
        {data.channels.length === 0 ? <Empty>还没有通知渠道。部署失败、备份失败、证书过期等事件可以推送到 Telegram、企业微信、邮件或 Webhook。</Empty> : (
          <div className="table-wrap">
            <table>
              <thead><tr><th>名称</th><th>类型</th><th>最近发送</th><th></th></tr></thead>
              <tbody>
                {data.channels.map((c: any) => (
                  <tr key={c.id}>
                    <td style={{ fontWeight: 600 }}>{c.name}{!c.enabled && <span className="badge off" style={{ marginLeft: 6 }}>停用</span>}</td>
                    <td>{kindName[c.kind]}</td>
                    <td>{c.lastSentAt ? <span className="row"><span className={'badge ' + (c.lastStatus === 'ok' ? 'ok' : 'err')} title={c.lastStatus}>{c.lastStatus === 'ok' ? '成功' : '失败'}</span><span className="muted">{ago(c.lastSentAt)}</span></span> : <span className="muted">—</span>}</td>
                    <td>
                      <div className="row" style={{ justifyContent: 'flex-end' }}>
                        <button className="btn btn-sm" onClick={async () => { try { const r = await post(`/notify/channels/${c.id}/test`); r.ok ? toast.ok(r.message) : toast.err(r.message); reload() } catch (e) { toast.err(e) } }}>发送测试</button>
                        <button className="btn btn-sm btn-ghost btn-icon" aria-label="编辑" onClick={() => setEdit({ id: c.id, name: c.name, kind: c.kind, config: { ...c.config }, enabled: c.enabled })}><Icon name="edit" size={16} /></button>
                        <button className="btn btn-sm btn-ghost btn-icon" aria-label="删除" onClick={() => setRm(c)}><Icon name="trash" size={16} /></button>
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Card>
      {data.channels.length > 0 && (
        <Card title="事件订阅" actions={<button className="btn btn-sm btn-primary" disabled={!dirty} onClick={async () => { try { await put('/notify/rules', rules); toast.ok('已保存'); reload() } catch (e) { toast.err(e) } }}>保存</button>} pad={false}>
          <div className="table-wrap">
            <table>
              <thead><tr><th>事件</th>{data.channels.map((c: any) => <th key={c.id}>{c.name}</th>)}</tr></thead>
              <tbody>
                {data.events.map((ev: any) => (
                  <tr key={ev.id}>
                    <td>{ev.name}</td>
                    {data.channels.map((c: any) => (
                      <td key={c.id}><input type="checkbox" aria-label={`${ev.name} → ${c.name}`} style={{ width: 16, height: 16, accentColor: '#B54708' }} checked={(rules[ev.id] || []).includes(c.id)} onChange={() => toggle(ev.id, c.id)} /></td>
                    ))}
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <p className="label" style={{ padding: '0 16px 12px', margin: 0 }}>同一对象的同类事件 30 分钟内只通知一次。</p>
        </Card>
      )}
      {edit && <ChannelModal c={edit} onClose={() => setEdit(null)} onSaved={() => { setEdit(null); reload() }} />}
      {rm && <Confirm title={`删除渠道 ${rm.name}`} danger confirmText="删除" onClose={() => setRm(null)} onConfirm={async () => { await del(`/notify/channels/${rm.id}`); reload() }}>该渠道的订阅也会一并删除。</Confirm>}
    </>
  )
}

function ChannelModal({ c, onClose, onSaved }: { c: Channel; onClose: () => void; onSaved: () => void }) {
  const [f, setF] = useState<Channel>(c)
  const [err, setErr] = useState<unknown>()
  const cf = f.config
  const setC = (patch: any) => setF({ ...f, config: { ...cf, ...patch } })
  const keep = c.id ? '留空保持不变' : undefined
  return (
    <Modal title={c.id ? '编辑通知渠道' : '添加通知渠道'} onClose={onClose} footer={<><button className="btn" onClick={onClose}>取消</button><button className="btn btn-primary" onClick={async () => { try { c.id ? await put(`/notify/channels/${c.id}`, f) : await post('/notify/channels', f); onSaved() } catch (e) { setErr(e) } }}>保存</button></>}>
      <Field label="名称"><input className="input" value={f.name} onChange={(e) => setF({ ...f, name: e.target.value })} /></Field>
      <Field label="类型">
        <select className="select" value={f.kind} disabled={!!c.id} onChange={(e) => setF({ ...f, kind: e.target.value as Channel['kind'], config: {} })}>
          {Object.entries(kindName).map(([k, l]) => <option key={k} value={k}>{l}</option>)}
        </select>
      </Field>
      {f.kind === 'telegram' && (
        <>
          <Field label="Bot Token" hint={keep}><input className="input mono" value={cf.botToken || ''} onChange={(e) => setC({ botToken: e.target.value })} autoComplete="off" /></Field>
          <Field label="Chat ID"><input className="input mono" value={cf.chatId || ''} onChange={(e) => setC({ chatId: e.target.value })} /></Field>
        </>
      )}
      {f.kind === 'wecom' && <Field label="Webhook 地址" hint="群机器人的 webhook URL"><input className="input mono" value={cf.url || ''} onChange={(e) => setC({ url: e.target.value })} /></Field>}
      {f.kind === 'webhook' && (
        <>
          <Field label="URL" hint="POST JSON：type、title、body、target、time"><input className="input mono" value={cf.url || ''} onChange={(e) => setC({ url: e.target.value })} /></Field>
          <Field label="签名密钥（可选）" hint={'设置后请求头带 X-Panel-Signature: sha256=HMAC。' + (keep ? keep : '')}><input className="input mono" type="password" value={cf.secret || ''} onChange={(e) => setC({ secret: e.target.value })} autoComplete="new-password" /></Field>
        </>
      )}
      {f.kind === 'email' && (
        <div className="form-grid">
          <Field label="SMTP 服务器"><input className="input mono" value={cf.host || ''} onChange={(e) => setC({ host: e.target.value })} /></Field>
          <Field label="端口" hint="465 使用 TLS，其它使用 STARTTLS"><input className="input mono" value={cf.port || ''} onChange={(e) => setC({ port: Number(e.target.value) || 0 })} placeholder="465" /></Field>
          <Field label="用户名"><input className="input" value={cf.username || ''} onChange={(e) => setC({ username: e.target.value })} autoComplete="off" /></Field>
          <Field label="密码" hint={keep}><input className="input" type="password" value={cf.password || ''} onChange={(e) => setC({ password: e.target.value })} autoComplete="new-password" /></Field>
          <Field label="发件人"><input className="input" value={cf.from || ''} onChange={(e) => setC({ from: e.target.value })} /></Field>
          <Field label="收件人" hint="多个用逗号分隔"><input className="input" value={cf.to || ''} onChange={(e) => setC({ to: e.target.value })} /></Field>
        </div>
      )}
      <label className="check"><input type="checkbox" checked={f.enabled} onChange={(e) => setF({ ...f, enabled: e.target.checked })} />启用</label>
      <ErrorBox error={err} />
    </Modal>
  )
}

function BackupKey() {
  const { data, error } = useLoad(() => get<any>('/backup-key'), [])
  const [identity, setIdentity] = useState<string | null>(null)
  const [confirm, setConfirm] = useState(false)
  if (!data) return error ? <ErrorBox error={error} /> : <Loading />
  return (
    <Card title="备份加密密钥（age）">
      <div className="col" style={{ gap: 14 }}>
        <p style={{ margin: 0 }}>加密的备份使用面板的 age 密钥对加密。<b>请把私钥离线保存一份</b>：如果服务器损坏，没有私钥就无法解密异地备份。</p>
        <SecretBox label="公钥（接收者）" value={data.recipient} />
        {identity ? <SecretBox label="私钥（妥善保管，不要分享）" value={identity} /> : <div><button className="btn" onClick={() => setConfirm(true)}><Icon name="key" />显示私钥</button></div>}
        <p className="label" style={{ margin: 0 }}>手动解密：<code className="mono">age -d -i key.txt backup.zst.age | zstd -d &gt; backup</code></p>
      </div>
      {confirm && <Confirm title="显示备份私钥" confirmText="显示" onClose={() => setConfirm(false)} onConfirm={async () => { const r = await get('/backup-key?reveal=1'); setIdentity(r.identity) }}>查看私钥会记录到审计日志。请确认周围环境安全。</Confirm>}
    </Card>
  )
}
