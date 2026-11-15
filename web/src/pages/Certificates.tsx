import { useState } from 'react'
import { dateTime, del, get, post } from '../api'
import { useCan } from '../auth'
import { Confirm, Empty, ErrorBox, Field, Icon, Loading, Modal, Page, useLoad, useToast } from '../components/ui'

export default function Certificates() {
  const { data, error, reload } = useLoad(() => get<any[]>('/certificates'), [], 60000)
  const can = useCan()
  const [upload, setUpload] = useState(false)
  const [rm, setRm] = useState<any>(null)
  return (
    <Page title="证书" sub="Let's Encrypt 证书由 Caddy 自动续期；也可以上传自有证书（如通配符证书）" actions={can.admin && <button className="btn btn-primary" onClick={() => setUpload(true)}><Icon name="plus" />上传证书</button>}>
      <ErrorBox error={error} />
      <section className="card">
        {!data ? <Loading /> : data.length === 0 ? <Empty>还没有证书。添加站点后 Caddy 会自动申请。</Empty> : (
          <div className="table-wrap">
            <table>
              <thead><tr><th>域名</th><th>剩余</th><th>到期</th><th>签发者</th><th>来源</th><th>使用站点</th><th></th></tr></thead>
              <tbody>
                {data.map((c) => (
                  <tr key={c.source + c.id + c.serial}>
                    <td className="mono" style={{ fontSize: 13 }}>{c.domains.join(', ')}{c.name && <div className="label">{c.name}</div>}</td>
                    <td>{c.source === 'internal' ? <span className="badge ok">自动续期</span> : <span className={'badge ' + (c.daysLeft <= 7 ? 'err' : c.daysLeft <= 14 ? 'warn' : 'ok')}>{c.daysLeft} 天</span>}</td>
                    <td className="nowrap muted">{dateTime(Date.parse(c.notAfter) / 1000)}</td>
                    <td>{c.issuer}</td>
                    <td>{c.source === 'uploaded' ? '上传' : c.source === 'internal' ? '本地 CA' : '自动（ACME）'}<div className="label">{c.keyType}</div></td>
                    <td className="mono" style={{ fontSize: 12 }}>{c.sites?.join(', ') || '—'}</td>
                    <td>{can.admin && c.source === 'uploaded' && <button className="btn btn-sm btn-ghost btn-icon" aria-label="删除证书" onClick={() => setRm(c)}><Icon name="trash" size={16} /></button>}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>
      {upload && <UploadModal onClose={() => setUpload(false)} onDone={() => { setUpload(false); reload() }} />}
      {rm && <Confirm title="删除上传的证书" danger confirmText="删除" onClose={() => setRm(null)} onConfirm={async () => { await del(`/certificates/${rm.id}`); reload() }}>使用该证书的站点将改为自动申请（通配符域名会失败）。</Confirm>}
    </Page>
  )
}

function UploadModal({ onClose, onDone }: { onClose: () => void; onDone: () => void }) {
  const toast = useToast()
  const [f, setF] = useState({ name: '', cert: '', key: '' })
  const [err, setErr] = useState<unknown>()
  const readFile = (k: 'cert' | 'key') => (e: React.ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0]
    if (file) file.text().then((t) => setF((x) => ({ ...x, [k]: t })))
  }
  return (
    <Modal title="上传证书" wide onClose={onClose} footer={<><button className="btn" onClick={onClose}>取消</button><button className="btn btn-primary" onClick={async () => { try { const r = await post('/certificates', f); toast.ok('已上传：' + (r.domains || []).join(', ')); if (r.warning) toast.err(r.warning); onDone() } catch (e) { setErr(e) } }}>上传</button></>}>
      <Field label="名称"><input className="input" value={f.name} onChange={(e) => setF({ ...f, name: e.target.value })} placeholder="example.com 通配符" /></Field>
      <Field label="证书链（PEM，fullchain）" hint={<input type="file" accept=".pem,.crt,.cer" onChange={readFile('cert')} />}>
        <textarea className="textarea mono" rows={6} value={f.cert} onChange={(e) => setF({ ...f, cert: e.target.value })} placeholder="-----BEGIN CERTIFICATE-----" />
      </Field>
      <Field label="私钥（PEM）" hint={<input type="file" accept=".pem,.key" onChange={readFile('key')} />}>
        <textarea className="textarea mono" rows={6} value={f.key} onChange={(e) => setF({ ...f, key: e.target.value })} placeholder="-----BEGIN PRIVATE KEY-----" />
      </Field>
      <p className="label" style={{ margin: 0 }}>私钥加密保存，只推送给 Caddy，不会再次显示。</p>
      <ErrorBox error={err} />
    </Modal>
  )
}
