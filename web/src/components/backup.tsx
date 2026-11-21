import { useEffect, useState } from 'react'
import { ago, bytes, dateTime, del, get, post } from '../api'
import { useCan } from '../auth'
import { isEngineCompatible } from './backupMeta'
import { PanelRestoreModal } from './PanelRestoreModal'
import { Combobox, ComboOption, Confirm, Empty, ErrorBox, Field, Icon, Loading, Modal, Select, Status, useJobLog, useLoad, useToast } from './ui'
import { MobileCard, ResponsiveTable } from './ResponsiveList'

export type BackupSource = { kind: 'database' | 'volume' | 'dir' | 'panel'; instanceId?: number; database?: string; volume?: string; projectId?: number; path?: string }

export function sourceText(s: BackupSource, names?: { db?: Record<number, string>; project?: Record<number, string> }) {
  switch (s.kind) {
    case 'database':
      if (!s.instanceId && !s.database) return '数据库（外部导入）'
      return `数据库 ${s.instanceId ? names?.db?.[s.instanceId] || '#' + s.instanceId : '外部'}${s.database ? ' / ' + s.database : ''}`
    case 'volume':
      return s.volume ? `数据卷 ${s.volume}` : '数据卷（外部导入）'
    case 'dir':
      if (!s.projectId && !s.path) return '项目目录（外部导入）'
      return `目录 ${s.projectId ? names?.project?.[s.projectId] || '#' + s.projectId : '外部'}${s.path ? '/' + s.path : ''}`
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
        <Select value={sid || ''} onChange={(v) => setStorageId(Number(v))}>
          {(storages || []).map((s) => <option key={s.id} value={s.id}>{s.name}（{({ s3: 'S3', local: '本地', sftp: 'SFTP', webdav: 'WebDAV', gdrive: 'Google Drive' } as Record<string, string>)[s.kind] || s.kind}）</option>)}
        </Select>
      </Field>
      <label className="check"><input type="checkbox" checked={encrypt} onChange={(e) => setEncrypt(e.target.checked)} />使用 age 加密</label>
      <ErrorBox error={err} />
    </Modal>
  )
}

export function SourcePicker({ value, onChange, sources }: { value: BackupSource; onChange: (s: BackupSource) => void; sources: any }) {
  if (!sources) return <div className="muted">加载备份来源…</div>
  const dbi = sources.databases?.find((d: any) => d.id === value.instanceId)
  const proj = sources.projects?.find((p: any) => p.id === value.projectId)
  const dirOpts: ComboOption[] = (proj?.dirs || []).map((d: any) => ({
    value: d.path,
    label: d.label,
    hint: d.hint,
  }))
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
            <Select value={value.instanceId || ''} onChange={(v) => onChange({ kind: 'database', instanceId: Number(v), database: '' })}>
              <option value="">选择实例</option>
              {sources.databases.map((d: any) => <option key={d.id} value={d.id}>{d.name}</option>)}
            </Select>
          </Field>
          <Field label="库">
            <Select value={value.database || ''} onChange={(v) => onChange({ ...value, database: v })}>
              <option value="">选择库</option>
              {(dbi?.databases || []).map((n: string) => <option key={n}>{n}</option>)}
            </Select>
          </Field>
        </div>
      )}
      {value.kind === 'volume' && (
        <Field label="数据卷">
          <Select value={value.volume || ''} onChange={(v) => onChange({ kind: 'volume', volume: v })}>
            <option value="">选择数据卷</option>
            {sources.volumes.map((v: any) => <option key={v.name} value={v.name}>{v.name}{v.project ? `（${v.project}）` : ''}</option>)}
          </Select>
        </Field>
      )}
      {value.kind === 'dir' && (
        <div className="form-grid">
          <Field label="项目">
            <Select
              value={value.projectId || ''}
              onChange={(v) => {
                const pid = Number(v)
                const next = sources.projects?.find((p: any) => p.id === pid)
                onChange({ ...value, kind: 'dir', projectId: pid, path: next?.dirs?.[0]?.path || '' })
              }}
            >
              <option value="">选择项目</option>
              {sources.projects.map((p: any) => <option key={p.id} value={p.id}>{p.name}</option>)}
            </Select>
          </Field>
          <Field
            div={dirOpts.length > 0}
            label="相对路径"
            hint={dirOpts.length > 0 ? `已识别 ${dirOpts.length} 个目录，可从列表选择或直接输入` : '项目目录下的子目录，如 data、uploads'}
          >
            {dirOpts.length > 0 ? (
              <Combobox
                mono
                aria-label="相对路径"
                value={value.path || ''}
                placeholder="data"
                options={dirOpts}
                onChange={(path) => onChange({ ...value, kind: 'dir', path })}
              />
            ) : (
              <input
                className="input mono"
                value={value.path || ''}
                placeholder="data"
                onChange={(e) => onChange({ ...value, kind: 'dir', path: e.target.value })}
              />
            )}
          </Field>
        </div>
      )}
      {value.kind === 'panel' && <div className="muted">备份面板的 SQLite 数据库（项目、站点、用户、加密的密钥等）。</div>}
    </div>
  )
}

export function RestoreModal({ backup, onClose }: { backup: any; onClose: () => void }) {
  const s: BackupSource = backup.source || { kind: 'database' }
  if (s.kind === 'panel' || backup.format === 'sqlite') {
    return <PanelRestoreModal backup={backup} onClose={onClose} />
  }
  return <DataRestoreModal backup={backup} onClose={onClose} />
}

function DataRestoreModal({ backup, onClose }: { backup: any; onClose: () => void }) {
  const s: BackupSource = backup.source || { kind: 'database' }
  const openJob = useJobLog()
  const { data: sources } = useLoad(() => get<any>('/backup-sources'), [])

  const isTarImported = backup.format === 'tar' && backup.origin && backup.origin !== 'backup'
  const [kind, setKind] = useState<'database' | 'volume' | 'dir'>(s.kind === 'volume' || s.kind === 'dir' ? s.kind : 'database')
  const [mode, setMode] = useState<'new' | 'overwrite'>('new')
  const [instanceId, setInstanceId] = useState<number>(s.instanceId || 0)
  const [database, setDatabase] = useState(s.database ? `${s.database}_restore` : 'restored_db')
  const [sourceDatabase, setSourceDatabase] = useState(s.database || '')
  const [volume, setVolume] = useState(s.volume ? `${s.volume}-restore` : '')
  const [projectId, setProjectId] = useState<number>(s.projectId || 0)
  const [dirPath, setDirPath] = useState(s.path ? `${s.path}-restore` : 'restored')
  const [ageIdentity, setAgeIdentity] = useState('')
  const [confirm, setConfirm] = useState('')
  const [err, setErr] = useState<unknown>()
  const [busy, setBusy] = useState(false)

  const compatibleDBs: any[] = (sources?.databases || []).filter((d: any) => isEngineCompatible(backup.format || '', d.engine))
  const volumes: any[] = sources?.volumes || []
  const projects: any[] = sources?.projects || []

  useEffect(() => {
    if (!sources) return
    if (kind === 'database') {
      const hasCurrent = compatibleDBs.some((d) => d.id === instanceId)
      if (!hasCurrent && compatibleDBs.length > 0) {
        setInstanceId(compatibleDBs[0].id)
      }
    } else if (kind === 'dir') {
      const hasProj = projects.some((p) => p.id === projectId)
      if (!hasProj && projects.length > 0) {
        setProjectId(projects[0].id)
      }
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [sources, kind])

  const selectedDB = compatibleDBs.find((d) => d.id === instanceId)
  const selectedProj = projects.find((p) => p.id === projectId)
  const isRedis = kind === 'database' && (selectedDB?.engine === 'redis' || backup.format === 'rdb')
  const effectiveMode: 'new' | 'overwrite' = isRedis ? 'overwrite' : mode

  const switchMode = (next: 'new' | 'overwrite') => {
    setMode(next)
    setConfirm('')
    if (kind === 'database') {
      if (next === 'overwrite') {
        const dbs: string[] = selectedDB?.databases || []
        if (s.database && dbs.includes(s.database)) setDatabase(s.database)
        else if (dbs.length > 0) setDatabase(dbs[0])
        else if (s.database) setDatabase(s.database)
      } else {
        setDatabase(s.database ? `${s.database}_restore` : 'restored_db')
      }
    } else if (kind === 'volume') {
      if (next === 'overwrite') {
        if (s.volume && volumes.some((v) => v.name === s.volume)) setVolume(s.volume)
        else if (volumes.length > 0) setVolume(volumes[0].name)
      } else {
        setVolume(s.volume ? `${s.volume}-restore` : '')
      }
    } else if (kind === 'dir') {
      if (next === 'overwrite') {
        setDirPath(s.path || selectedProj?.dirs?.[0]?.path || 'data')
      } else {
        setDirPath(s.path ? `${s.path}-restore` : 'restored')
      }
    }
  }

  const confirmExpected =
    kind === 'database'
      ? isRedis
        ? selectedDB?.name || ''
        : database.trim()
      : kind === 'volume'
        ? volume.trim()
        : selectedProj
          ? `${selectedProj.name}/${dirPath.trim().replace(/^\/+|\/+$/g, '')}`
          : dirPath.trim()

  const needsConfirm = effectiveMode === 'overwrite'
  const targetReady =
    kind === 'database'
      ? !!selectedDB && (isRedis || database.trim() !== '')
      : kind === 'volume'
        ? volume.trim() !== ''
        : !!selectedProj && dirPath.trim() !== ''

  const submit = async () => {
    setBusy(true)
    setErr(undefined)
    try {
      const payload: Record<string, any> = {
        kind,
        mode: effectiveMode,
        confirm: confirm.trim(),
        ageIdentity: ageIdentity.trim() || undefined,
      }
      if (kind === 'database') {
        payload.instanceId = instanceId
        payload.database = isRedis ? 'all' : database.trim()
        payload.target = payload.database
        if (selectedDB?.engine === 'mongo' && sourceDatabase.trim()) {
          payload.sourceDatabase = sourceDatabase.trim()
        }
      } else if (kind === 'volume') {
        payload.volume = volume.trim()
        payload.target = volume.trim()
      } else {
        payload.projectId = projectId
        payload.path = dirPath.trim()
        payload.target = dirPath.trim()
      }
      const r = await post(`/backups/${backup.id}/restore`, payload)
      openJob(r.jobId, '恢复 ' + backup.label)
      onClose()
    } catch (e) {
      setErr(e)
    } finally {
      setBusy(false)
    }
  }

  const dirOpts: ComboOption[] = (selectedProj?.dirs || []).map((d: any) => ({ value: d.path, label: d.label, hint: d.hint }))

  return (
    <Modal
      title="恢复备份"
      wide
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose}>取消</button>
          <button
            className={'btn ' + (needsConfirm ? 'btn-danger solid' : 'btn-primary')}
            disabled={busy || !targetReady || (needsConfirm && confirm.trim() !== confirmExpected)}
            onClick={submit}
          >
            {busy ? '提交中…' : '开始恢复'}
          </button>
        </>
      }
    >
      <dl className="kv">
        <dt>来源</dt><dd>{sourceText(s)}</dd>
        <dt>时间</dt><dd>{ago(backup.startedAt)}</dd>
        <dt>大小 / 格式</dt><dd>{bytes(backup.size)} · <span className="mono">{backup.format || '—'}</span>{backup.encrypted && ' · 已加密'}</dd>
        <dt>对象</dt><dd className="mono" style={{ fontSize: 12, overflowWrap: 'anywhere' }}>{backup.objectKey}</dd>
      </dl>

      {!sources ? (
        <Loading />
      ) : (
        <>
          {isTarImported && (
            <div className="seg" role="group" aria-label="恢复目标类型">
              <button type="button" className={kind === 'volume' ? 'on' : ''} onClick={() => { setKind('volume'); setConfirm('') }}>恢复到数据卷</button>
              <button type="button" className={kind === 'dir' ? 'on' : ''} onClick={() => { setKind('dir'); setConfirm('') }}>恢复到项目目录</button>
            </div>
          )}

          {kind === 'database' && (
            <>
              {compatibleDBs.length === 0 ? (
                <div className="alert warn"><Icon name="alert" />当前面板中没有与格式 <code>{backup.format}</code> 兼容的数据库实例，请先在「数据库」页创建对应实例。</div>
              ) : (
                <>
                  <Field label="目标数据库实例" hint="支持跨同引擎实例恢复">
                    <Select value={instanceId || ''} onChange={(v) => { setInstanceId(Number(v)); setConfirm('') }}>
                      {compatibleDBs.map((d) => <option key={d.id} value={d.id}>{d.name}（{d.engine}）</option>)}
                    </Select>
                  </Field>
                  {isRedis ? (
                    <>
                      <div className="alert err"><Icon name="alert" /><div>Redis 只能整个实例覆盖恢复：会短暂停止 <b>{confirmExpected}</b>，用备份 RDB 替换全部数据并重建 AOF，然后重新启动。开始前面板会自动先备份一次当前数据。</div></div>
                      <Field label={<>输入实例名 <code className="mono">{confirmExpected}</code> 确认</>}>
                        <input className="input mono" value={confirm} onChange={(e) => setConfirm(e.target.value)} />
                      </Field>
                    </>
                  ) : (
                    <>
                      <div className="seg" role="group" aria-label="恢复方式">
                        <button type="button" className={mode === 'new' ? 'on' : ''} onClick={() => switchMode('new')}>恢复到新库（推荐）</button>
                        <button type="button" className={mode === 'overwrite' ? 'on' : ''} onClick={() => switchMode('overwrite')}>覆盖已有库</button>
                      </div>
                      {mode === 'new' ? (
                        <Field label="新库名" hint="在目标实例中创建新库并导入，验证后再切换应用连接">
                          <input className="input mono" value={database} onChange={(e) => setDatabase(e.target.value)} />
                        </Field>
                      ) : (
                        <>
                          <Field label="选择或输入要覆盖的库">
                            {(selectedDB?.databases || []).length > 0 ? (
                              <Combobox
                                mono
                                value={database}
                                onChange={(v) => { setDatabase(v); setConfirm('') }}
                                options={(selectedDB?.databases || []).map((n: string) => ({ value: n, label: n }))}
                                placeholder="选择或输入库名"
                              />
                            ) : (
                              <input className="input mono" value={database} onChange={(e) => { setDatabase(e.target.value); setConfirm('') }} />
                            )}
                          </Field>
                          <div className="alert err">
                            <Icon name="alert" />
                            <div>
                              覆盖会清空 <b>{database || '目标库'}</b> 的现有数据（开始前若库已存在会自动先备份一次当前数据）。
                              {selectedDB?.engine === 'postgres' && backup.format === 'sql' && ' PostgreSQL 明文 SQL 覆盖会断开该库活跃连接并重置用户 Schema，非库所有者用户的表级授权需按需重授。'}
                            </div>
                          </div>
                          <Field label={<>输入库名 <code className="mono">{confirmExpected}</code> 确认</>}>
                            <input className="input mono" value={confirm} onChange={(e) => setConfirm(e.target.value)} />
                          </Field>
                        </>
                      )}
                      {selectedDB?.engine === 'mongo' && (
                        <Field label="归档内原库名（可选）" hint="留空时自动将归档内单库集合映射到目标库；若归档包含多库请填写原库名">
                          <input className="input mono" value={sourceDatabase} onChange={(e) => setSourceDatabase(e.target.value)} placeholder="留空自动映射" />
                        </Field>
                      )}
                    </>
                  )}
                </>
              )}
            </>
          )}

          {kind === 'volume' && (
            <>
              <div className="seg" role="group" aria-label="恢复方式">
                <button type="button" className={mode === 'new' ? 'on' : ''} onClick={() => switchMode('new')}>恢复到新数据卷（推荐）</button>
                <button type="button" className={mode === 'overwrite' ? 'on' : ''} onClick={() => switchMode('overwrite')}>覆盖已有数据卷</button>
              </div>
              {mode === 'new' ? (
                <Field label="新数据卷名" hint="创建新卷并解包，验证后再修改 compose.yml 挂载">
                  <input className="input mono" value={volume} onChange={(e) => setVolume(e.target.value)} placeholder="如 app_data_restore" />
                </Field>
              ) : (
                <>
                  <Field label="要覆盖的数据卷">
                    <Select value={volume} onChange={(v) => { setVolume(v); setConfirm('') }}>
                      <option value="">选择数据卷</option>
                      {volumes.map((v) => <option key={v.name} value={v.name}>{v.name}{v.project ? `（${v.project}）` : ''}</option>)}
                    </Select>
                  </Field>
                  <div className="alert err"><Icon name="alert" /><div>覆盖会先自动备份当前卷，再清空并解包写入。若该卷正被运行中的容器挂载，面板会拒绝覆盖，请先停止对应项目/容器。</div></div>
                  <Field label={<>输入数据卷名 <code className="mono">{confirmExpected}</code> 确认</>}>
                    <input className="input mono" value={confirm} onChange={(e) => setConfirm(e.target.value)} />
                  </Field>
                </>
              )}
            </>
          )}

          {kind === 'dir' && (
            <>
              {projects.length === 0 ? (
                <div className="alert warn"><Icon name="alert" />当前面板中还没有应用项目，请先创建或导入项目。</div>
              ) : (
                <>
                  <Field label="目标项目">
                    <Select value={projectId || ''} onChange={(v) => { setProjectId(Number(v)); setConfirm('') }}>
                      {projects.map((p) => <option key={p.id} value={p.id}>{p.name}</option>)}
                    </Select>
                  </Field>
                  <div className="seg" role="group" aria-label="恢复方式">
                    <button type="button" className={mode === 'new' ? 'on' : ''} onClick={() => switchMode('new')}>恢复到新目录（推荐）</button>
                    <button type="button" className={mode === 'overwrite' ? 'on' : ''} onClick={() => switchMode('overwrite')}>覆盖已有目录</button>
                  </div>
                  <Field div={dirOpts.length > 0 && mode === 'overwrite'} label="目标相对路径（项目目录下）" hint={mode === 'new' ? '目录必须尚不存在' : '覆盖前会自动先备份原目录，并采用临时目录原子替换'}>
                    {dirOpts.length > 0 && mode === 'overwrite' ? (
                      <Combobox mono value={dirPath} onChange={(v) => { setDirPath(v); setConfirm('') }} options={dirOpts} placeholder="data" />
                    ) : (
                      <input className="input mono" value={dirPath} onChange={(e) => { setDirPath(e.target.value); setConfirm('') }} placeholder="data-restore" />
                    )}
                  </Field>
                  {mode === 'overwrite' && (
                    <>
                      <div className="alert err"><Icon name="alert" /><div>覆盖前若检测到运行中的容器正通过 bind 挂载使用该目录（含父/子目录），面板会拒绝覆盖以防容器滞留已删除文件句柄，请先停止相关项目。</div></div>
                      <Field label={<>输入 <code className="mono">{confirmExpected}</code> 确认</>}>
                        <input className="input mono" value={confirm} onChange={(e) => setConfirm(e.target.value)} />
                      </Field>
                    </>
                  )}
                </>
              )}
            </>
          )}

          {backup.encrypted && (
            <Field label="临时 age 解密私钥（可选）" hint="默认依次尝试当前面板密钥与历史解密密钥环；仅本次恢复临时解密其他机器密钥时填写，不落库">
              <input className="input mono" type="password" value={ageIdentity} onChange={(e) => setAgeIdentity(e.target.value)} placeholder="AGE-SECRET-KEY-1…" autoComplete="off" />
            </Field>
          )}
        </>
      )}
      <ErrorBox error={err} />
    </Modal>
  )
}

function VerifyBackupModal({ backup, onClose }: { backup: any; onClose: () => void }) {
  const openJob = useJobLog()
  const toast = useToast()
  const [ageIdentity, setAgeIdentity] = useState('')
  const [busy, setBusy] = useState(false)
  const run = async () => {
    setBusy(true)
    try {
      const r = await post(`/backups/${backup.id}/verify`, ageIdentity.trim() ? { ageIdentity: ageIdentity.trim() } : {})
      openJob(r.jobId, '校验备份')
      onClose()
    } catch (e) {
      toast.err(e)
    } finally {
      setBusy(false)
    }
  }
  return (
    <Modal
      title="校验备份完整性与解密"
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose}>取消</button>
          <button className="btn btn-primary" disabled={busy} onClick={run}>{busy ? '提交中…' : '开始校验'}</button>
        </>
      }
    >
      <p style={{ margin: 0 }}>将下载 <code className="mono">{backup.objectKey}</code>，核对 SHA-256（扫描导入记录校验通过后自动回填）并完整尝试解密与解压。</p>
      <Field label="临时 age 解密私钥（可选）" hint="留空默认使用面板密钥及历史解密密钥环">
        <input className="input mono" type="password" value={ageIdentity} onChange={(e) => setAgeIdentity(e.target.value)} placeholder="AGE-SECRET-KEY-1…" autoComplete="off" />
      </Field>
    </Modal>
  )
}

const originBadge: Record<string, string> = { upload: '上传导入', scan: '存储导入' }

export function BackupTable({ list, reload, names }: { list: any[]; reload: () => void; names?: { db?: Record<number, string>; project?: Record<number, string> } }) {
  const can = useCan()
  const toast = useToast()
  const openJob = useJobLog()
  const [restore, setRestore] = useState<any>(null)
  const [verifyTarget, setVerifyTarget] = useState<any>(null)
  const [rm, setRm] = useState<any>(null)
  const [deleteFile, setDeleteFile] = useState(true)
  if (list.length === 0) return <Empty>没有备份记录。</Empty>
  const openDelete = (b: any) => {
    setDeleteFile(b.origin !== 'scan')
    setRm(b)
  }
  const renderActions = (b: any) => (
    <>
      {b.jobId && <button className="btn btn-sm btn-ghost" onClick={() => openJob(b.jobId)}>日志</button>}
      {b.status === 'success' && (
        <>
          {can.operator && (
            <button
              className="btn btn-sm"
              onClick={async () => {
                if (b.encrypted) {
                  setVerifyTarget(b)
                  return
                }
                try {
                  const r = await post(`/backups/${b.id}/verify`)
                  openJob(r.jobId, '校验备份')
                } catch (e) {
                  toast.err(e)
                }
              }}
            >
              校验
            </button>
          )}
          {can.admin && <a className="btn btn-sm" href={`/api/backups/${b.id}/download${b.encrypted ? '?plain=1' : ''}`} title={b.encrypted ? '下载解密后的文件' : '下载'}><Icon name="download" size={16} /></a>}
          {can.admin && <button className="btn btn-sm" onClick={() => setRestore(b)}>恢复</button>}
        </>
      )}
      {can.admin && <button className="btn btn-sm btn-ghost btn-icon" aria-label="删除备份" onClick={() => openDelete(b)}><Icon name="trash" size={16} /></button>}
    </>
  )
  return (
    <>
      <ResponsiveTable
        table={
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
                      {b.origin && originBadge[b.origin] && <span className="badge off" style={{ marginLeft: 6 }}>{originBadge[b.origin]}</span>}
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
                        {renderActions(b)}
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        }
        cards={
          list.map((b) => (
            <MobileCard
              key={b.id}
              title={sourceText(b.source, names)}
              sub={<span className="mono">{b.objectKey || b.label}</span>}
              badges={
                <>
                  <Status s={b.status} />
                  {b.origin && originBadge[b.origin] && <span className="badge off">{originBadge[b.origin]}</span>}
                  {b.encrypted && <span className="badge off"><Icon name="lock" size={12} />加密</span>}
                  {b.verifiedAt > 0 && <span className="badge ok" title={'恢复演练通过：' + dateTime(b.verifiedAt)}><Icon name="check" size={12} />已演练</span>}
                </>
              }
              extra={b.error ? <div className="label" style={{ color: '#A41E14', overflowWrap: 'anywhere' }}>{b.error}</div> : undefined}
              meta={[
                { label: '时间', value: ago(b.startedAt) },
                { label: '大小', value: b.size ? bytes(b.size) : '—' },
                { label: '存储', value: b.storageName },
                { label: '保留', value: b.tiers || '—', mono: true },
              ]}
              actions={renderActions(b)}
            />
          ))
        }
      />
      {restore && <RestoreModal backup={restore} onClose={() => setRestore(null)} />}
      {verifyTarget && <VerifyBackupModal backup={verifyTarget} onClose={() => setVerifyTarget(null)} />}
      {rm && (
        <Confirm
          title="删除备份记录"
          danger
          confirmText="删除"
          onClose={() => setRm(null)}
          onConfirm={async () => {
            await del(`/backups/${rm.id}${deleteFile ? '' : '?keepFile=1'}`)
            reload()
          }}
        >
          <div className="col" style={{ gap: 10 }}>
            <div>将从面板记录中移除 <code className="mono">{rm.objectKey}</code>。</div>
            <label className="check">
              <input type="checkbox" checked={deleteFile} onChange={(e) => setDeleteFile(e.target.checked)} />
              同时删除存储中的备份文件（不可恢复）
            </label>
          </div>
        </Confirm>
      )}
    </>
  )
}
