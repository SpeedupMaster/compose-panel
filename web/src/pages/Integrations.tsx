import { useEffect, useRef, useState } from 'react'
import { ago, bytes, del, get, post, put } from '../api'
import { Card, Confirm, Empty, ErrorBox, Field, Icon, Loading, Modal, Page, SecretBox, Select, Tabs, useLoad, useToast } from '../components/ui'
import { MobileCard, ResponsiveTable } from '../components/ResponsiveList'

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

type Target = { id?: number; name: string; kind: 'local' | 's3' | 'sftp' | 'webdav' | 'gdrive'; config: any }
const storageKind: Record<string, string> = { s3: 'S3 兼容', local: '本地磁盘', sftp: 'SFTP', webdav: 'WebDAV', gdrive: 'Google Drive' }
function storageLocation(t: any) {
  const c = t.config || {}
  if (t.kind === 's3') return `${c.endpoint}/${c.bucket}/${c.prefix || ''}`
  if (t.kind === 'sftp') return `${c.username}@${c.host}:${c.port || 22}${c.path ? ' ' + c.path : ''}`
  if (t.kind === 'webdav') return c.url
  if (t.kind === 'gdrive') {
    const base = c.folderId ? `folders/${c.folderId}` : '根目录'
    const prefix = (c.prefix || '').replace(/^\/+|\/+$/g, '')
    return prefix ? `${base}/${prefix}` : base
  }
  return c.path
}

function Storage() {
  const { data, error, reload } = useLoad(() => get<any[]>('/storage'), [])
  const toast = useToast()
  const [edit, setEdit] = useState<Target | null>(null)
  const [rm, setRm] = useState<any>(null)
  if (!data) return error ? <ErrorBox error={error} /> : <Loading />
  return (
    <Card title="备份存储位置" actions={<button className="btn btn-sm btn-primary" onClick={() => setEdit({ name: '', kind: 's3', config: { region: 'auto', prefix: '' } })}><Icon name="plus" size={16} />添加</button>} pad={false}>
      <ResponsiveTable
        table={
          <div className="table-wrap">
            <table>
              <thead><tr><th>名称</th><th>类型</th><th>位置</th><th>备份</th><th>连接测试</th><th></th></tr></thead>
              <tbody>
                {data.map(({ target: t, backups, size, policies }) => (
                  <tr key={t.id}>
                    <td style={{ fontWeight: 600 }}>{t.name}</td>
                    <td>{storageKind[t.kind] || t.kind}</td>
                    <td className="mono" style={{ fontSize: 12, overflowWrap: 'anywhere' }}>{storageLocation(t)}</td>
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
        }
        cards={data.map(({ target: t, backups, size, policies }) => (
          <MobileCard
            key={t.id}
            title={t.name}
            sub={<span className="mono">{storageLocation(t)}</span>}
            badges={t.lastCheckAt ? <span className={'badge ' + (t.lastCheckOk ? 'ok' : 'err')} title={t.lastCheckMsg}>{t.lastCheckOk ? '正常' : '失败'} · {ago(t.lastCheckAt)}</span> : <span className="badge off">未测试</span>}
            meta={[
              { label: '类型', value: storageKind[t.kind] || t.kind },
              { label: '备份', value: `${backups} 份 · ${bytes(size)}` },
              { label: '策略', value: `${policies} 个策略`, full: true },
            ]}
            actions={
              <>
                <button className="btn btn-sm" onClick={async () => { try { const r = await post(`/storage/${t.id}/test`); r.ok ? toast.ok(`连接正常（${r.elapsedMs} ms）`) : toast.err(r.message); reload() } catch (e) { toast.err(e) } }}>测试连接</button>
                <button className="btn btn-sm btn-ghost btn-icon" aria-label="编辑" onClick={() => setEdit({ id: t.id, name: t.name, kind: t.kind, config: { ...t.config } })}><Icon name="edit" size={16} /></button>
                <button className="btn btn-sm btn-ghost btn-icon" aria-label="删除" onClick={() => setRm(t)}><Icon name="trash" size={16} /></button>
              </>
            }
          />
        ))}
      />
      {edit && <StorageModal t={edit} onClose={() => setEdit(null)} onSaved={() => { setEdit(null); reload() }} />}
      {rm && <Confirm title={`删除存储 ${rm.name}`} danger confirmText="删除" onClose={() => setRm(null)} onConfirm={async () => { await del(`/storage/${rm.id}`); reload() }}>只删除面板中的配置，存储中的备份文件不会被删除。仍被策略使用的存储无法删除。</Confirm>}
    </Card>
  )
}

function StorageModal({ t, onClose, onSaved }: { t: Target; onClose: () => void; onSaved: () => void }) {
  const toast = useToast()
  const [f, setF] = useState<Target>(t)
  const [err, setErr] = useState<unknown>()
  const [test, setTest] = useState<any>(null)
  const [authBusy, setAuthBusy] = useState(false)
  const [authNotice, setAuthNotice] = useState('')
  const authCleanupRef = useRef<(() => void) | null>(null)
  const c = f.config
  const setC = (patch: any) => setF((prev) => ({ ...prev, config: { ...prev.config, ...patch } }))
  const redirectUri = `${window.location.origin}/api/storage/gdrive/callback`

  useEffect(() => () => { authCleanupRef.current?.() }, [])

  const startGoogleAuth = async () => {
    setErr(undefined)
    setAuthNotice('')
    if (!c.clientId?.trim()) {
      setErr('请先填写 Client ID')
      return
    }
    if (!t.id && !c.clientSecret?.trim()) {
      setErr('请先填写 Client Secret')
      return
    }
    authCleanupRef.current?.()
    const popup = window.open('about:blank', 'compose-panel-gdrive-oauth', 'width=540,height=680,popup=yes')
    setAuthBusy(true)
    try {
      const { url, state } = await post<{ url: string; state: string }>('/storage/gdrive/auth-url', {
        id: t.id || 0,
        clientId: c.clientId || '',
        clientSecret: c.clientSecret || '',
        redirectUri,
      })
      if (popup && !popup.closed) popup.location.href = url
      else window.open(url, '_blank')

      let finished = false
      let checking = false
      let closeCheckTimer: number | undefined

      const cleanup = () => {
        finished = true
        window.removeEventListener('message', onMsg)
        ch?.close()
        window.clearInterval(pollTimer)
        if (closeCheckTimer !== undefined) window.clearTimeout(closeCheckTimer)
        authCleanupRef.current = null
        setAuthBusy(false)
      }

      const checkResult = async (finalTry = false) => {
        if (finished || checking) return
        checking = true
        try {
          const r = await post<{ status: string; refreshToken?: string; error?: string }>('/storage/gdrive/auth-result', { state })
          if (finished) return
          if (r.status === 'ok' && r.refreshToken) {
            setF((prev) => ({ ...prev, config: { ...prev.config, refreshToken: r.refreshToken } }))
            setAuthNotice('Google 授权成功，已填入 Refresh Token，请点击测试连接或保存。')
            toast.ok('Google Drive 授权成功')
            cleanup()
          } else if (r.status === 'error') {
            setErr(r.error || 'Google 授权失败')
            cleanup()
          } else if (finalTry) {
            cleanup()
          }
        } catch (e) {
          if (!finished && finalTry) {
            setErr(e)
            cleanup()
          }
        } finally {
          checking = false
        }
      }

      const onMsg = (ev: MessageEvent) => {
        if (ev.origin !== window.location.origin) return
        if (ev.data?.type === 'gdrive-oauth-done' && ev.data?.state === state) {
          void checkResult()
        }
      }
      window.addEventListener('message', onMsg)

      let ch: BroadcastChannel | null = null
      if ('BroadcastChannel' in window) {
        ch = new BroadcastChannel('compose-panel-gdrive-oauth')
        ch.onmessage = (ev) => {
          if (ev.data?.type === 'gdrive-oauth-done' && ev.data?.state === state) {
            void checkResult()
          }
        }
      }

      const pollTimer = window.setInterval(() => {
        if (popup && popup.closed && closeCheckTimer === undefined) {
          closeCheckTimer = window.setTimeout(() => { void checkResult(true) }, 800)
          return
        }
        void checkResult()
      }, 1500)

      authCleanupRef.current = cleanup
    } catch (e) {
      popup?.close()
      setAuthBusy(false)
      setErr(e)
    }
  }

  return (
    <Modal
      title={t.id ? '编辑存储' : '添加存储'}
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={async () => { try { setTest(await post('/storage/test', t.id ? { ...f, id: t.id } : f)) } catch (e) { setTest({ ok: false, message: (e as Error).message }) } }}>测试连接</button>
          <span className="spacer" />
          <button className="btn" onClick={onClose}>取消</button>
          <button className="btn btn-primary" onClick={async () => { try { t.id ? await put(`/storage/${t.id}`, f) : await post('/storage', f); onSaved() } catch (e) { setErr(e) } }}>保存</button>
        </>
      }
    >
      <Field label="名称"><input className="input" value={f.name} onChange={(e) => setF({ ...f, name: e.target.value })} /></Field>
      <div className="seg" role="group" aria-label="类型" style={{ flexWrap: 'wrap' }}>
        {(['s3', 'sftp', 'webdav', 'gdrive', 'local'] as const).map((k) => (
          <button
            key={k}
            type="button"
            className={f.kind === k ? 'on' : ''}
            disabled={!!t.id && t.kind !== k}
            onClick={() => {
              if (f.kind === k) return
              setTest(null)
              setErr(undefined)
              setAuthNotice('')
              setF((prev) => ({ ...prev, kind: k, config: t.id ? prev.config : k === 's3' ? { region: 'auto', prefix: '' } : {} }))
            }}
          >
            {storageKind[k]}
          </button>
        ))}
      </div>
      {f.kind === 's3' && <span className="label">Cloudflare R2、阿里云 OSS、腾讯云 COS、MinIO、Backblaze B2 等</span>}
      {f.kind === 'gdrive' && <span className="label">个人 Google Drive（OAuth 2.0）。请在 Google Cloud 启用 Drive API，并将 OAuth 同意屏幕发布为「正式版」以避免测试模式 7 天后失效。</span>}
      {f.kind === 'sftp' ? (
        <div className="form-grid">
          <Field label="主机"><input className="input mono" value={c.host || ''} onChange={(e) => setC({ host: e.target.value })} placeholder="backup.example.com" /></Field>
          <Field label="端口"><input className="input mono" value={c.port || ''} onChange={(e) => setC({ port: Number(e.target.value) || 0 })} placeholder="22" inputMode="numeric" /></Field>
          <Field label="用户名"><input className="input mono" value={c.username || ''} onChange={(e) => setC({ username: e.target.value })} autoComplete="off" /></Field>
          <Field label="远程目录" hint="相对路径基于用户主目录"><input className="input mono" value={c.path || ''} onChange={(e) => setC({ path: e.target.value })} placeholder="backups/panel" /></Field>
          <Field label="密码" hint={t.id ? '留空保持不变；可与私钥二选一' : '可与私钥二选一'}><input className="input mono" type="password" value={c.password === '••••••' ? '' : c.password || ''} onChange={(e) => setC({ password: e.target.value })} autoComplete="new-password" /></Field>
          <Field label="主机指纹（推荐）" hint="SHA256:…，留空则不校验；可用 ssh-keyscan 主机 | ssh-keygen -lf - 获取"><input className="input mono" value={c.hostKey || ''} onChange={(e) => setC({ hostKey: e.target.value })} placeholder="SHA256:…" /></Field>
          <div style={{ gridColumn: '1 / -1' }}>
            <Field label="私钥（OpenSSH 格式，可选）" hint={t.id && c.privateKey === '••••••' ? '已设置，留空保持不变' : '不支持带密码的私钥'}>
              <textarea className="textarea mono" rows={4} value={c.privateKey === '••••••' ? '' : c.privateKey || ''} onChange={(e) => setC({ privateKey: e.target.value })} placeholder="-----BEGIN OPENSSH PRIVATE KEY-----" />
            </Field>
          </div>
        </div>
      ) : f.kind === 'webdav' ? (
        <div className="form-grid">
          <div style={{ gridColumn: '1 / -1' }}>
            <Field label="WebDAV 地址" hint="目录的完整 URL，如 Nextcloud：https://cloud.example.com/remote.php/dav/files/me/backups/"><input className="input mono" value={c.url || ''} onChange={(e) => setC({ url: e.target.value })} /></Field>
          </div>
          <Field label="用户名"><input className="input mono" value={c.username || ''} onChange={(e) => setC({ username: e.target.value })} autoComplete="off" /></Field>
          <Field label="密码 / 应用密码" hint={t.id ? '留空保持不变' : undefined}><input className="input mono" type="password" value={c.password === '••••••' ? '' : c.password || ''} onChange={(e) => setC({ password: e.target.value })} autoComplete="new-password" /></Field>
          <label className="check"><input type="checkbox" checked={!!c.insecure} onChange={(e) => setC({ insecure: e.target.checked })} />跳过 TLS 证书校验（自签名证书）</label>
        </div>
      ) : f.kind === 'gdrive' ? (
        <div className="form-grid">
          <Field label="Client ID"><input className="input mono" value={c.clientId || ''} onChange={(e) => setC({ clientId: e.target.value })} placeholder="xxxx.apps.googleusercontent.com" autoComplete="off" /></Field>
          <Field label="Client Secret" hint={t.id ? '留空保持不变' : undefined}><input className="input mono" type="password" value={c.clientSecret === '••••••' ? '' : c.clientSecret || ''} onChange={(e) => setC({ clientSecret: e.target.value })} autoComplete="new-password" /></Field>
          <div style={{ gridColumn: '1 / -1' }}>
            <Field label="授权回调地址（Redirect URI）" hint="在 Google Cloud → 凭据 → OAuth 2.0 Web 客户端的「已获授权的重定向 URI」中添加此地址">
              <div className="row">
                <input className="input mono grow" readOnly value={redirectUri} onFocus={(e) => e.currentTarget.select()} />
                <button
                  type="button"
                  className="btn"
                  onClick={async () => {
                    try {
                      await navigator.clipboard.writeText(redirectUri)
                      toast.ok('已复制回调地址')
                    } catch {
                      toast.err('复制失败，请手动选中复制')
                    }
                  }}
                >
                  <Icon name="copy" size={15} />复制
                </button>
              </div>
            </Field>
          </div>
          <div style={{ gridColumn: '1 / -1' }}>
            <Field
              label="Refresh Token"
              hint={
                t.id && c.refreshToken === '••••••'
                  ? '已授权，留空保持不变；点击右侧按钮可重新授权，也可直接粘贴新的 Refresh Token'
                  : '点击右侧按钮跳转 Google 授权自动获取；若面板使用内网 IP/HTTP 无法回调，也可手动粘贴 Refresh Token'
              }
            >
              <div className="row">
                <input
                  className="input mono grow"
                  type="password"
                  value={c.refreshToken === '••••••' ? '' : c.refreshToken || ''}
                  onChange={(e) => { setAuthNotice(''); setC({ refreshToken: e.target.value }) }}
                  placeholder={t.id && c.refreshToken === '••••••' ? '已授权（留空保持不变）' : '1//…'}
                  autoComplete="new-password"
                />
                <button type="button" className="btn" disabled={authBusy} onClick={startGoogleAuth}>
                  <Icon name="external" size={15} />
                  {authBusy ? '等待授权…' : c.refreshToken ? '重新授权' : '前往 Google 授权'}
                </button>
              </div>
            </Field>
          </div>
          <Field label="目标文件夹 ID（可选）" hint="留空存到根目录；支持直接粘贴文件夹链接"><input className="input mono" value={c.folderId || ''} onChange={(e) => setC({ folderId: e.target.value })} placeholder="例如 1AbCdEf…" /></Field>
          <Field label="子目录前缀（可选）" hint="自动在目标目录下按层级创建"><input className="input mono" value={c.prefix || ''} onChange={(e) => setC({ prefix: e.target.value })} placeholder="compose-panel/" /></Field>
        </div>
      ) : f.kind === 'local' ? (
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
      {authNotice && <div className="alert ok">{authNotice}</div>}
      {test && <div className={'alert ' + (test.ok ? 'ok' : 'err')}>{test.ok ? `连接正常（${test.elapsedMs} ms）` : test.message}</div>}
      <ErrorBox error={err} />
    </Modal>
  )
}

type Channel = { id?: number; name: string; kind: 'webhook' | 'telegram' | 'wecom' | 'dingtalk' | 'feishu' | 'bark' | 'email'; config: any; enabled: boolean }
const kindName: Record<string, string> = { telegram: 'Telegram', wecom: '企业微信机器人', dingtalk: '钉钉机器人', feishu: '飞书机器人', bark: 'Bark（iOS 推送）', email: '邮件 (SMTP)', webhook: 'Webhook' }

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
        {data.channels.length === 0 ? <Empty>还没有通知渠道。部署失败、备份失败、容器崩溃、证书过期等事件可以推送到 Telegram、企业微信、钉钉、飞书、Bark、邮件或 Webhook。</Empty> : (
          <ResponsiveTable
            table={
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
            }
            cards={data.channels.map((c: any) => (
              <MobileCard
                key={c.id}
                title={c.name}
                sub={kindName[c.kind]}
                badges={
                  <>
                    {!c.enabled && <span className="badge off">停用</span>}
                    {c.lastSentAt ? (
                      <span className={'badge ' + (c.lastStatus === 'ok' ? 'ok' : 'err')} title={c.lastStatus}>
                        {c.lastStatus === 'ok' ? '成功' : '失败'} · {ago(c.lastSentAt)}
                      </span>
                    ) : (
                      <span className="badge off">未发送</span>
                    )}
                  </>
                }
                actions={
                  <>
                    <button className="btn btn-sm" onClick={async () => { try { const r = await post(`/notify/channels/${c.id}/test`); r.ok ? toast.ok(r.message) : toast.err(r.message); reload() } catch (e) { toast.err(e) } }}>发送测试</button>
                    <button className="btn btn-sm btn-ghost btn-icon" aria-label="编辑" onClick={() => setEdit({ id: c.id, name: c.name, kind: c.kind, config: { ...c.config }, enabled: c.enabled })}><Icon name="edit" size={16} /></button>
                    <button className="btn btn-sm btn-ghost btn-icon" aria-label="删除" onClick={() => setRm(c)}><Icon name="trash" size={16} /></button>
                  </>
                }
              />
            ))}
          />
        )}
      </Card>
      {data.channels.length > 0 && (
        <Card title="事件订阅" actions={<button className="btn btn-sm btn-primary" disabled={!dirty} onClick={async () => { try { await put('/notify/rules', rules); toast.ok('已保存'); reload() } catch (e) { toast.err(e) } }}>保存</button>} pad={false}>
          <ResponsiveTable
            table={
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
            }
            cards={data.events.map((ev: any) => (
              <MobileCard
                key={ev.id}
                tight
                title={ev.name}
                extra={
                  <div className="row wrap" style={{ gap: '4px 14px' }}>
                    {data.channels.map((c: any) => (
                      <label key={c.id} className="check">
                        <input type="checkbox" checked={(rules[ev.id] || []).includes(c.id)} onChange={() => toggle(ev.id, c.id)} />
                        <span>{c.name}</span>
                      </label>
                    ))}
                  </div>
                }
              />
            ))}
          />
          <p className="label" style={{ padding: '8px 16px 12px', margin: 0 }}>同一对象的同类事件 30 分钟内只通知一次。</p>
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
        <Select value={f.kind} disabled={!!c.id} onChange={(v) => setF({ ...f, kind: v as Channel['kind'], config: {} })}>
          {Object.entries(kindName).map(([k, l]) => <option key={k} value={k}>{l}</option>)}
        </Select>
      </Field>
      {f.kind === 'telegram' && (
        <>
          <Field label="Bot Token" hint={keep}><input className="input mono" value={cf.botToken || ''} onChange={(e) => setC({ botToken: e.target.value })} autoComplete="off" /></Field>
          <Field label="Chat ID"><input className="input mono" value={cf.chatId || ''} onChange={(e) => setC({ chatId: e.target.value })} /></Field>
        </>
      )}
      {f.kind === 'wecom' && <Field label="Webhook 地址" hint="群机器人的 webhook URL"><input className="input mono" value={cf.url || ''} onChange={(e) => setC({ url: e.target.value })} /></Field>}
      {(f.kind === 'dingtalk' || f.kind === 'feishu') && (
        <>
          <Field label="Webhook 地址" hint={f.kind === 'dingtalk' ? 'https://oapi.dingtalk.com/robot/send?access_token=…' : 'https://open.feishu.cn/open-apis/bot/v2/hook/…'}><input className="input mono" value={cf.url || ''} onChange={(e) => setC({ url: e.target.value })} /></Field>
          <Field label="加签密钥（可选）" hint={'机器人安全设置中的「加签」密钥。' + (keep || '')}><input className="input mono" type="password" value={cf.secret || ''} onChange={(e) => setC({ secret: e.target.value })} autoComplete="new-password" /></Field>
          {f.kind === 'dingtalk' && <span className="label">如果机器人使用「自定义关键词」安全设置，请把关键词设为事件标题中会出现的文字，或改用加签。</span>}
        </>
      )}
      {f.kind === 'bark' && <Field label="设备地址" hint="Bark App 中复制的地址，如 https://api.day.app/你的Key（自建服务器同理）"><input className="input mono" value={cf.url || ''} onChange={(e) => setC({ url: e.target.value })} /></Field>}
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
