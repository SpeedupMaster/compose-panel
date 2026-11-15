import { useState } from 'react'
import { ago, bytes, dateTime, del, get, post } from '../api'
import { useCan } from '../auth'
import { Confirm, Empty, ErrorBox, Field, Icon, Modal, Status, useJobLog, useLoad, useToast } from './ui'

export type BackupSource = { kind: 'database' | 'volume' | 'dir' | 'panel'; instanceId?: number; database?: string; volume?: string; projectId?: number; path?: string }

export function sourceText(s: BackupSource, names?: { db?: Record<number, string>; project?: Record<number, string> }) {
  switch (s.kind) {
    case 'database':
      return `数据库 ${names?.db?.[s.instanceId!] || '#' + s.instanceId} / ${s.database}`
    case 'volume':
      return `数据卷 ${s.volume}`
    case 'dir':
      return `目录 ${names?.project?.[s.projectId!] || '#' + s.projectId}/${s.path}`
    case 'panel':
      return '面板数据'
  }
  return s.kind
}

export function useStorages() {
  return useLoad(() => get<any[]>('/storage').then((l) => l.map((x) => x.target)), [])
}

// Ad-hoc backup modal.
export function BackupNowModal({ source, onClose, onDone }: { source?: BackupSource; onClose: () => void; onDone?: () => void }) {
  const { data: storages } = useStorages()
  const { data: sources } = useLoad(() => (source ? Promise.resolve(null) : get<any>('/backup-sources')), [])
  const openJob = useJobLog()
  const [src, setSrc] = useState<BackupSource>(source || { kind: 'database' })
  const [storageId, setStorageId] = useState(0)
  const [encrypt, setEncrypt] = useState(true)
  const [err, setErr] = useState<unknown>()
  const sid = storageId || storages?.[0]?.id
  const submit = async () => {
    try {
      const r = await post('/backups/run', { source: src, storageId: sid, encrypt })
      openJob(r.jobId, '立即备份')
      onDone?.()
      onClose()
    } catch (e) {
      setErr(e)
    }
  }
  return (
    <Modal title="立即备份" onClose={onClose} footer={<><button className="btn" onClick={onClose}>取消</button><button className="btn btn-primary" disabled={!sid} onClick={submit}>开始备份</button></>}>
      {source ? <div>备份对象：<b>{sourceText(source)}</b></div> : <SourcePicker value={src} onChange={setSrc} sources={sources} />}
      <Field label="存储位置">
        <select className="select" value={sid || ''} onChange={(e) => setStorageId(Number(e.target.value))}>
          {(storages || []).map((s) => <option key={s.id} value={s.id}>{s.name}（{({ s3: 'S3', local: '本地', sftp: 'SFTP', webdav: 'WebDAV' } as Record<string, string>)[s.kind] || s.kind}）</option>)}
        </select>
      </Field>
      <label className="check"><input type="checkbox" checked={encrypt} onChange={(e) => setEncrypt(e.target.checked)} />使用 age 加密</label>
      <ErrorBox error={err} />
    </Modal>
  )
}

export function SourcePicker({ value, onChange, sources }: { value: BackupSource; onChange: (s: BackupSource) => void; sources: any }) {
  if (!sources) return <div className="muted">加载备份来源…</div>
  const dbi = sources.databases?.find((d: any) => d.id === value.instanceId)
  return (
    <div className="col" style={{ gap: 12 }}>
      <div className="seg" role="group" aria-label="备份类型">
        {[['database', '数据库'], ['volume', '数据卷'], ['dir', '项目目录'], ['panel', '面板数据']].map(([k, l]) => (
          <button key={k} className={value.kind === k ? 'on' : ''} onClick={() => onChange({ kind: k as BackupSource['kind'] })}>{l}</button>
        ))}
      </div>
      {value.kind === 'database' && (
        <div className="form-grid">
          <Field label="实例">
            <select className="select" value={value.instanceId || ''} onChange={(e) => onChange({ kind: 'database', instanceId: Number(e.target.value), database: '' })}>
              <option value="">选择实例</option>
              {sources.databases.map((d: any) => <option key={d.id} value={d.id}>{d.name}</option>)}
            </select>
          </Field>
          <Field label="库">
            <select className="select" value={value.database || ''} onChange={(e) => onChange({ ...value, database: e.target.value })}>
              <option value="">选择库</option>
              {(dbi?.databases || []).map((n: string) => <option key={n}>{n}</option>)}
            </select>
          </Field>
        </div>
      )}
      {value.kind === 'volume' && (
        <Field label="数据卷">
          <select className="select" value={value.volume || ''} onChange={(e) => onChange({ kind: 'volume', volume: e.target.value })}>
            <option value="">选择数据卷</option>
            {sources.volumes.map((v: any) => <option key={v.name} value={v.name}>{v.name}{v.project ? `（${v.project}）` : ''}</option>)}
          </select>
        </Field>
      )}
      {value.kind === 'dir' && (
        <div className="form-grid">
          <Field label="项目">
            <select className="select" value={value.projectId || ''} onChange={(e) => onChange({ ...value, kind: 'dir', projectId: Number(e.target.value) })}>
              <option value="">选择项目</option>
              {sources.projects.map((p: any) => <option key={p.id} value={p.id}>{p.name}</option>)}
            </select>
          </Field>
          <Field label="相对路径" hint="项目目录下的子目录，如 data、uploads">
            <input className="input mono" value={value.path || ''} onChange={(e) => onChange({ ...value, kind: 'dir', path: e.target.value })} />
          </Field>
        </div>
      )}
      {value.kind === 'panel' && <div className="muted">备份面板的 SQLite 数据库（项目、站点、用户、加密的密钥等）。</div>}
    </div>
  )
}

export function RestoreModal({ backup, onClose }: { backup: any; onClose: () => void }) {
  const s: BackupSource = backup.source
  const openJob = useJobLog()
  const [mode, setMode] = useState<'new' | 'overwrite'>('new')
  const [target, setTarget] = useState(s.kind === 'database' ? `${s.database}_restore` : s.kind === 'volume' ? `${s.volume}-restore` : `${s.path}-restore`)
  const [confirm, setConfirm] = useState('')
  const [err, setErr] = useState<unknown>()
  const [busy, setBusy] = useState(false)
  const isRedis = s.kind === 'database' && backup.format === 'rdb'
  const unsupported = s.kind === 'panel'
  const { data: inst } = useLoad(async () => (isRedis ? (await get<any>(`/databases/${s.instanceId}`)).instance : null), [s.instanceId, isRedis])
  const confirmName = isRedis ? inst?.name || '…' : s.database
  const submit = async () => {
    setBusy(true)
    setErr(undefined)
    try {
      const r = await post(`/backups/${backup.id}/restore`, { mode: isRedis ? 'overwrite' : mode, target, confirm })
      openJob(r.jobId, '恢复 ' + backup.label)
      onClose()
    } catch (e) {
      setErr(e)
    } finally {
      setBusy(false)
    }
  }
  return (
    <Modal
      title="恢复备份"
      onClose={onClose}
      footer={!unsupported && <><button className="btn" onClick={onClose}>取消</button><button className={'btn ' + (mode === 'overwrite' || isRedis ? 'btn-danger solid' : 'btn-primary')} disabled={busy || ((mode === 'overwrite' || isRedis) && confirm !== confirmName)} onClick={submit}>{busy ? '提交中…' : '开始恢复'}</button></>}
    >
      <dl className="kv">
        <dt>来源</dt><dd>{sourceText(s)}</dd>
        <dt>时间</dt><dd>{ago(backup.startedAt)}</dd>
        <dt>大小</dt><dd>{bytes(backup.size)}{backup.encrypted && ' · 已加密'}</dd>
        <dt>对象</dt><dd className="mono" style={{ fontSize: 12 }}>{backup.objectKey}</dd>
      </dl>
      {unsupported ? (
        <div className="alert warn">面板数据备份不支持在线恢复，请下载后替换 panel.db 并重启面板。</div>
      ) : isRedis ? (
        <>
          <div className="alert err"><Icon name="alert" /><div>Redis 只能整个实例覆盖恢复：会短暂停止 <b>{confirmName}</b>，用备份的 RDB 替换全部数据并重建 AOF，然后重新启动。开始前面板会自动先备份一次当前数据。</div></div>
          <Field label={<>输入实例名 <code className="mono">{confirmName}</code> 确认</>}>
            <input className="input mono" value={confirm} onChange={(e) => setConfirm(e.target.value)} />
          </Field>
        </>
      ) : (
        <>
          {s.kind === 'database' && (
            <div className="seg" role="group" aria-label="恢复方式">
              <button className={mode === 'new' ? 'on' : ''} onClick={() => setMode('new')}>恢复到新库（推荐）</button>
              <button className={mode === 'overwrite' ? 'on' : ''} onClick={() => setMode('overwrite')}>覆盖原库</button>
            </div>
          )}
          {mode === 'new' ? (
            <Field label={s.kind === 'database' ? '新库名' : s.kind === 'volume' ? '新数据卷名' : '目标目录（项目目录下）'} hint="恢复到新位置，验证后再切换应用配置">
              <input className="input mono" value={target} onChange={(e) => setTarget(e.target.value)} />
            </Field>
          ) : (
            <>
              <div className="alert err"><Icon name="alert" /><div>覆盖会清空 <b>{s.database}</b> 的现有数据。开始前面板会自动先备份一次当前数据。</div></div>
              <Field label={<>输入库名 <code className="mono">{s.database}</code> 确认</>}>
                <input className="input mono" value={confirm} onChange={(e) => setConfirm(e.target.value)} />
              </Field>
            </>
          )}
        </>
      )}
      <ErrorBox error={err} />
    </Modal>
  )
}

export function BackupTable({ list, reload, names }: { list: any[]; reload: () => void; names?: { db?: Record<number, string>; project?: Record<number, string> } }) {
  const can = useCan()
  const toast = useToast()
  const openJob = useJobLog()
  const [restore, setRestore] = useState<any>(null)
  const [rm, setRm] = useState<any>(null)
  if (list.length === 0) return <Empty>没有备份记录。</Empty>
  return (
    <div className="table-wrap">
      <table>
        <thead><tr><th>对象</th><th>状态</th><th>时间</th><th>大小</th><th>存储</th><th>保留</th><th></th></tr></thead>
        <tbody>
          {list.map((b) => (
            <tr key={b.id}>
              <td>
                <div style={{ fontWeight: 500 }}>{sourceText(b.source, names)}</div>
                <div className="label mono ellipsis" style={{ maxWidth: 320 }} title={b.objectKey}>{b.objectKey || b.label}</div>
              </td>
              <td>
                <Status s={b.status} />
                {b.encrypted && <span className="badge off" style={{ marginLeft: 6 }}><Icon name="lock" size={12} />加密</span>}
                {b.verifiedAt > 0 && <span className="badge ok" style={{ marginLeft: 6 }} title={'恢复演练通过：' + dateTime(b.verifiedAt)}><Icon name="check" size={12} />已演练</span>}
                {b.error && <div className="label ellipsis" style={{ maxWidth: 240, color: '#A41E14' }} title={b.error}>{b.error}</div>}
              </td>
              <td className="nowrap muted">{ago(b.startedAt)}</td>
              <td className="nowrap">{b.size ? bytes(b.size) : '—'}</td>
              <td>{b.storageName}</td>
              <td className="mono" style={{ fontSize: 12 }}>{b.tiers || '—'}</td>
              <td>
                <div className="row" style={{ justifyContent: 'flex-end' }}>
                  {b.jobId && <button className="btn btn-sm btn-ghost" onClick={() => openJob(b.jobId)}>日志</button>}
                  {b.status === 'success' && (
                    <>
                      {can.operator && <button className="btn btn-sm" onClick={async () => { try { const r = await post(`/backups/${b.id}/verify`); openJob(r.jobId, '校验备份') } catch (e) { toast.err(e) } }}>校验</button>}
                      {can.admin && <a className="btn btn-sm" href={`/api/backups/${b.id}/download${b.encrypted ? '?plain=1' : ''}`} title={b.encrypted ? '下载解密后的文件' : '下载'}><Icon name="download" size={16} /></a>}
                      {can.admin && <button className="btn btn-sm" onClick={() => setRestore(b)}>恢复</button>}
                    </>
                  )}
                  {can.admin && <button className="btn btn-sm btn-ghost btn-icon" aria-label="删除备份" onClick={() => setRm(b)}><Icon name="trash" size={16} /></button>}
                </div>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      {restore && <RestoreModal backup={restore} onClose={() => setRestore(null)} />}
      {rm && (
        <Confirm title="删除备份" danger confirmText="删除" onClose={() => setRm(null)} onConfirm={async () => { await del(`/backups/${rm.id}`); reload() }}>
          同时删除存储中的备份文件 <code className="mono">{rm.objectKey}</code>，不可恢复。
        </Confirm>
      )}
    </div>
  )
}
