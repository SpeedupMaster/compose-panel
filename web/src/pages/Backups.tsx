import { useEffect, useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import { ago, bytes, del, get, post, put } from '../api'
import { useCan } from '../auth'
import { BackupNowModal, BackupSource, BackupTable, RestoreModal, SourcePicker, sourceText, useStorages } from '../components/backup'
import { ImportUploadModal, ScanStorageModal } from '../components/BackupImportModals'
import { Drills } from './Drills'
import { MobileCard, ResponsiveTable, joinInline } from '../components/ResponsiveList'
import { Confirm, Empty, ErrorBox, Field, Icon, Loading, Modal, Page, Select, Status, Tabs, useJobLog, useLoad, useToast } from '../components/ui'

type Policy = {
  id?: number
  name: string
  source: BackupSource
  schedule: string
  keepDaily: number
  keepWeekly: number
  keepMonthly: number
  keepYearly: number
  storageId: number
  encrypt: boolean
  ageRecipient: string
  enabled: boolean
}

const presets: [string, string][] = [['0 3 * * *', '每天 03:00'], ['0 */6 * * *', '每 6 小时'], ['0 * * * *', '每小时'], ['0 4 * * 0', '每周日 04:00']]

export default function Backups() {
  const [params, setParams] = useSearchParams()
  const [tab, setTab] = useState<'policies' | 'records' | 'drills'>('policies')
  const can = useCan()
  const { data: policies, error, reload } = useLoad(() => get<any[]>('/backup-policies'), [], 15000)
  const { data: sources } = useLoad(() => get<any>('/backup-sources'), [])
  const [status, setStatus] = useState('')
  const [q, setQ] = useState('')
  const { data: records, reload: reloadRecords } = useLoad(() => get<any[]>(`/backups?limit=300${status ? '&status=' + status : ''}${q ? '&q=' + encodeURIComponent(q) : ''}`), [status, q], 15000)
  const [edit, setEdit] = useState<Policy | null>(null)
  const [now, setNow] = useState(false)
  const [uploadOpen, setUploadOpen] = useState(false)
  const [scanOpen, setScanOpen] = useState(false)
  const [quickRestore, setQuickRestore] = useState<any>(null)
  const [rm, setRm] = useState<any>(null)
  const openJob = useJobLog()
  const toast = useToast()

  useEffect(() => {
    const k = params.get('new')
    if (k === 'database') {
      setEdit(blankPolicy({ kind: 'database', instanceId: Number(params.get('instance')) || undefined }))
      setParams({}, { replace: true })
    } else if (params.get('volume')) {
      setEdit(blankPolicy({ kind: 'volume', volume: params.get('volume')! }))
      setParams({}, { replace: true })
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  const names = {
    db: Object.fromEntries((sources?.databases || []).map((d: any) => [d.id, d.name])),
    project: Object.fromEntries((sources?.projects || []).map((p: any) => [p.id, p.name])),
  }
  const failed24 = (records || []).filter((b) => b.status === 'failed' && b.startedAt > Date.now() / 1000 - 86400).length
  const total = (records || []).filter((b) => b.status === 'success').reduce((a, b) => a + b.size, 0)

  return (
    <Page
      title="备份"
      actions={can.operator && (
        <>
          {can.admin && <button className="btn" onClick={() => setUploadOpen(true)}><Icon name="upload" size={16} />上传备份</button>}
          {can.admin && <button className="btn" onClick={() => setScanOpen(true)}><Icon name="search" size={16} />从存储导入</button>}
          <button className="btn" onClick={() => setNow(true)}>立即备份</button>
          <button className="btn btn-primary" onClick={() => setEdit(blankPolicy({ kind: 'database' }))}><Icon name="plus" />新建策略</button>
        </>
      )}
    >
      <div className="grid">
        <div className="card stat"><span className="label">备份策略</span><div className="big">{policies?.length ?? '—'}</div><span className="label">{(policies || []).filter((p) => p.enabled).length} 个启用</span></div>
        <div className="card stat"><span className="label">24 小时失败</span><div className="big" style={{ color: failed24 ? '#A41E14' : undefined }}>{failed24}</div></div>
        <div className="card stat"><span className="label">备份总量（最近 300 条）</span><div className="big">{bytes(total)}</div></div>
      </div>
      <Tabs tabs={[['policies', '策略'], ['records', '备份记录'], ['drills', '恢复演练']]} value={tab} onChange={setTab} />
      <ErrorBox error={error} />
      {tab === 'policies' && (
        <section className="card">
          {!policies ? <Loading /> : policies.length === 0 ? (
            <Empty action={can.operator && <button className="btn btn-primary" onClick={() => setEdit(blankPolicy({ kind: 'database' }))}>新建策略</button>}>还没有备份策略。建议为每个数据库和重要数据卷设置每日备份。</Empty>
          ) : (
            <ResponsiveTable
              table={
                <div className="table-wrap">
                  <table>
                    <thead><tr><th>名称</th><th>对象</th><th>计划</th><th>保留</th><th>存储</th><th>上次</th><th>下次</th><th></th></tr></thead>
                    <tbody>
                      {policies.map((p) => (
                        <tr key={p.id}>
                          <td style={{ fontWeight: 600 }}>{p.name}{!p.enabled && <span className="badge off" style={{ marginLeft: 6 }}>停用</span>}</td>
                          <td>{sourceText(p.source, names)}</td>
                          <td className="mono">{p.schedule}</td>
                          <td className="mono" style={{ fontSize: 12 }}>{p.keepDaily}D {p.keepWeekly}W {p.keepMonthly}M {p.keepYearly}Y</td>
                          <td>{p.storageName}{p.encrypt && <Icon name="lock" size={14} />}</td>
                          <td className="nowrap">{p.lastRun ? <span className="row"><Status s={p.lastRun.status} /><span className="muted">{ago(p.lastRun.startedAt)}</span></span> : <span className="muted">—</span>}</td>
                          <td className="muted nowrap">{p.enabled && p.nextRun ? ago(p.nextRun) : '—'}</td>
                          <td>
                            <div className="row" style={{ justifyContent: 'flex-end' }}>
                              {can.operator && <button className="btn btn-sm" onClick={async () => { try { const r = await post(`/backup-policies/${p.id}/run`); openJob(r.jobId, `备份 ${p.name}`) } catch (e) { toast.err(e) } }}>运行</button>}
                              {can.operator && <button className="btn btn-sm btn-ghost btn-icon" aria-label="编辑" onClick={() => setEdit({ ...p })}><Icon name="edit" size={16} /></button>}
                              {can.admin && <button className="btn btn-sm btn-ghost btn-icon" aria-label="删除" onClick={() => setRm(p)}><Icon name="trash" size={16} /></button>}
                            </div>
                          </td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              }
              cards={policies.map((p) => (
                <MobileCard
                  key={p.id}
                  title={p.name}
                  sub={sourceText(p.source, names)}
                  badges={
                    <>
                      {!p.enabled && <span className="badge off">停用</span>}
                      {p.encrypt && <span className="badge off"><Icon name="lock" size={12} />加密</span>}
                      {p.lastRun && <Status s={p.lastRun.status} />}
                    </>
                  }
                  meta={[
                    { label: '计划', value: p.schedule, mono: true },
                    { label: '存储', value: p.storageName },
                    { label: '保留', value: `${p.keepDaily}D ${p.keepWeekly}W ${p.keepMonthly}M ${p.keepYearly}Y`, mono: true },
                    { label: '时间', value: joinInline([p.lastRun && `上次 ${ago(p.lastRun.startedAt)}`, p.enabled && p.nextRun && `下次 ${ago(p.nextRun)}`]) || '—' },
                  ]}
                  actions={
                    can.operator || can.admin ? (
                      <>
                        {can.operator && <button className="btn btn-sm" onClick={async () => { try { const r = await post(`/backup-policies/${p.id}/run`); openJob(r.jobId, `备份 ${p.name}`) } catch (e) { toast.err(e) } }}>运行</button>}
                        {can.operator && <button className="btn btn-sm btn-ghost btn-icon" aria-label="编辑" onClick={() => setEdit({ ...p })}><Icon name="edit" size={16} /></button>}
                        {can.admin && <button className="btn btn-sm btn-ghost btn-icon" aria-label="删除" onClick={() => setRm(p)}><Icon name="trash" size={16} /></button>}
                      </>
                    ) : undefined
                  }
                />
              ))}
            />
          )}
        </section>
      )}
      {tab === 'records' && (
        <section className="card">
          <div className="card-head">
            <div className="row wrap">
              <input className="input filter-input" placeholder="搜索" aria-label="搜索备份" value={q} onChange={(e) => setQ(e.target.value)} />
              <Select className="filter-select" aria-label="状态" value={status} onChange={(v) => setStatus(v)}>
                <option value="">全部状态</option>
                <option value="success">成功</option>
                <option value="failed">失败</option>
                <option value="running">进行中</option>
              </Select>
            </div>
          </div>
          {records ? <BackupTable list={records} reload={reloadRecords} names={names} /> : <Loading />}
        </section>
      )}
      {tab === 'drills' && <Drills policies={policies || []} />}
      {edit && <PolicyModal p={edit} sources={sources} onClose={() => setEdit(null)} onSaved={() => { setEdit(null); reload() }} />}
      {now && <BackupNowModal onClose={() => setNow(false)} onDone={reloadRecords} />}
      {uploadOpen && (
        <ImportUploadModal
          onClose={() => setUploadOpen(false)}
          onImported={(b, openRestore) => {
            setTab('records')
            reloadRecords()
            if (openRestore && b) setQuickRestore(b)
          }}
        />
      )}
      {scanOpen && (
        <ScanStorageModal
          onClose={() => setScanOpen(false)}
          onImported={(b, openRestore) => {
            setTab('records')
            reloadRecords()
            if (openRestore && b) setQuickRestore(b)
          }}
        />
      )}
      {quickRestore && <RestoreModal backup={quickRestore} onClose={() => setQuickRestore(null)} />}
      {rm && <Confirm title={`删除策略 ${rm.name}`} danger confirmText="删除" onClose={() => setRm(null)} onConfirm={async () => { await del(`/backup-policies/${rm.id}`); reload() }}>已有的备份文件会保留，可在备份记录中单独删除。</Confirm>}
    </Page>
  )
}

function blankPolicy(source: BackupSource): Policy {
  return { name: '', source, schedule: '0 3 * * *', keepDaily: 7, keepWeekly: 4, keepMonthly: 6, keepYearly: 0, storageId: 0, encrypt: true, ageRecipient: '', enabled: true }
}

function PolicyModal({ p, sources, onClose, onSaved }: { p: Policy; sources: any; onClose: () => void; onSaved: () => void }) {
  const { data: storages } = useStorages()
  const [f, setF] = useState<Policy>(p)
  const [err, setErr] = useState<unknown>()
  const sid = f.storageId || storages?.[0]?.id || 0
  const submit = async () => {
    setErr(undefined)
    try {
      const body = { ...f, storageId: sid, name: f.name || autoName(f.source, sources) }
      if (f.id) await put(`/backup-policies/${f.id}`, body)
      else await post('/backup-policies', body)
      onSaved()
    } catch (e) {
      setErr(e)
    }
  }
  return (
    <Modal title={f.id ? '编辑备份策略' : '新建备份策略'} wide onClose={onClose} footer={<><button className="btn" onClick={onClose}>取消</button><button className="btn btn-primary" onClick={submit}>保存</button></>}>
      {f.id ? <div>备份对象：<b>{sourceText(f.source)}</b></div> : <SourcePicker value={f.source} onChange={(source) => setF({ ...f, source })} sources={sources} />}
      <div className="form-grid" style={{ gridTemplateColumns: 'repeat(auto-fit, minmax(min(300px, 100%), 1fr))' }}>
        <Field label="名称" hint="留空自动生成"><input className="input" value={f.name} onChange={(e) => setF({ ...f, name: e.target.value })} /></Field>
        <Field label="存储位置">
          <Select value={sid} onChange={(v) => setF({ ...f, storageId: Number(v) })}>
            {(storages || []).map((s) => <option key={s.id} value={s.id}>{s.name}（{({ s3: 'S3', local: '本地', sftp: 'SFTP', webdav: 'WebDAV', gdrive: 'Google Drive' } as Record<string, string>)[s.kind] || s.kind}）</option>)}
          </Select>
        </Field>
        <Field label="计划（cron，5 段）" hint="按面板时区执行">
          <input className="input mono" value={f.schedule} onChange={(e) => setF({ ...f, schedule: e.target.value })} />
        </Field>
        <Field label="常用计划" div>
          <div className="row wrap" style={{ gap: 6, minHeight: 36 }}>{presets.map(([c, l]) => <button key={c} type="button" className={'btn btn-sm' + (f.schedule === c ? ' btn-primary' : '')} onClick={() => setF({ ...f, schedule: c })}>{l}</button>)}</div>
        </Field>
      </div>
      <div>
        <div className="label" style={{ marginBottom: 6 }}>保留策略（祖父-父-子）：每个周期保留最新的一份，最新一份始终保留</div>
        <div className="form-grid" style={{ gridTemplateColumns: 'repeat(auto-fit, minmax(120px, 1fr))' }}>
          {([['keepDaily', '按天'], ['keepWeekly', '按周'], ['keepMonthly', '按月'], ['keepYearly', '按年']] as const).map(([k, l]) => (
            <Field key={k} label={l}><input className="input" type="number" min={0} value={f[k]} onChange={(e) => setF({ ...f, [k]: Number(e.target.value) })} /></Field>
          ))}
        </div>
      </div>
      <label className="check"><input type="checkbox" checked={f.encrypt} onChange={(e) => setF({ ...f, encrypt: e.target.checked })} />使用 age 加密（面板密钥）</label>
      {f.encrypt && (
        <Field label="额外接收者公钥（可选）" hint="每行一个 age1… 公钥。持有对应私钥的人也能解密，适合把密钥离线保管">
          <textarea className="textarea mono" rows={3} value={f.ageRecipient} onChange={(e) => setF({ ...f, ageRecipient: e.target.value })} />
        </Field>
      )}
      <label className="check"><input type="checkbox" checked={f.enabled} onChange={(e) => setF({ ...f, enabled: e.target.checked })} />启用</label>
      <ErrorBox error={err} />
    </Modal>
  )
}

function autoName(s: BackupSource, sources: any) {
  if (s.kind === 'database') return `${sources?.databases?.find((d: any) => d.id === s.instanceId)?.name || 'db'}-${s.database}`
  if (s.kind === 'volume') return `卷-${s.volume}`
  if (s.kind === 'dir') return `${sources?.projects?.find((p: any) => p.id === s.projectId)?.name || 'dir'}-${s.path}`
  return '面板数据'
}
