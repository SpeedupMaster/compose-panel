import { useEffect, useState } from 'react'
import { ago, bytes, post, qs, upload } from '../api'
import { BackupKind, formatLabels, inferUploadMeta, kindLabels } from './backupMeta'
import { useStorages } from './backup'
import { MobileCard, ResponsiveTable } from './ResponsiveList'
import { Empty, ErrorBox, Field, FilePicker, Icon, Loading, Modal, Select, useToast } from './ui'

const storageKindName: Record<string, string> = { s3: 'S3', local: '本地', sftp: 'SFTP', webdav: 'WebDAV', gdrive: 'Google Drive' }

export function ImportUploadModal({ onClose, onImported }: { onClose: () => void; onImported: (backup: any, openRestore: boolean) => void }) {
  const { data: storages } = useStorages()
  const toast = useToast()
  const [file, setFile] = useState<File | null>(null)
  const [storageId, setStorageId] = useState(0)
  const [kind, setKind] = useState<BackupKind>('database')
  const [format, setFormat] = useState('pgdump')
  const [label, setLabel] = useState('')
  const [openRestore, setOpenRestore] = useState(true)
  const [progress, setProgress] = useState<number | null>(null)
  const [err, setErr] = useState<unknown>()

  const sid = storageId || storages?.[0]?.id || 0

  const onFile = (f: File | null) => {
    setFile(f)
    setErr(undefined)
    if (!f) return
    const meta = inferUploadMeta(f.name)
    setKind(meta.kind)
    setFormat(meta.format || (meta.kind === 'panel' ? 'sqlite' : meta.kind === 'database' ? 'sql' : 'tar'))
    setLabel(meta.label)
  }

  const onKindChange = (k: BackupKind) => {
    setKind(k)
    if (k === 'volume' || k === 'dir') setFormat('tar')
    else if (k === 'panel') setFormat('sqlite')
    else if (!['pgdump', 'sql', 'archive', 'rdb'].includes(format)) setFormat('sql')
  }

  const submit = async () => {
    if (!file || !sid) return
    setErr(undefined)
    setProgress(0)
    try {
      const res = await upload<{ backup: any }>(
        'POST',
        '/backups/import/upload' + qs({ name: file.name, storageId: sid, kind, format, label: label.trim() || undefined }),
        file,
        setProgress,
      )
      toast.ok('备份文件已导入')
      onImported(res.backup, openRestore)
      onClose()
    } catch (e) {
      setErr(e)
    } finally {
      setProgress(null)
    }
  }

  return (
    <Modal
      title="上传外部备份文件"
      wide
      onClose={onClose}
      footer={
        <>
          <button className="btn" disabled={progress != null} onClick={onClose}>取消</button>
          <button className="btn btn-primary" disabled={!file || !sid || !format || progress != null} onClick={submit}>
            <Icon name="upload" size={16} />{progress != null ? `上传中 ${progress}%` : openRestore ? '上传并恢复' : '开始上传'}
          </button>
        </>
      }
    >
      <Field label="选择备份文件" div>
        <FilePicker
          file={file}
          onChange={onFile}
          disabled={progress != null}
          accept=".zst,.age,.gz,.tgz,.sql,.dump,.pgdump,.backup,.archive,.rdb,.tar,.sqlite,.db,.sqlite3"
          hint="支持面板导出的 .zst / .zst.age 归档，也支持 .sql / .pgdump / .archive / .rdb / .tar / .db 等原文件或 .gz 压缩文件"
        />
      </Field>
      <div className="form-grid" style={{ gridTemplateColumns: 'repeat(auto-fit, minmax(min(240px, 100%), 1fr))' }}>
        <Field label="保存到存储位置">
          <Select value={sid || ''} onChange={(v) => setStorageId(Number(v))}>
            {(storages || []).map((s) => <option key={s.id} value={s.id}>{s.name}（{storageKindName[s.kind] || s.kind}）</option>)}
          </Select>
        </Field>
        <Field label="备注标签" hint="留空按文件名自动生成">
          <input className="input" value={label} onChange={(e) => setLabel(e.target.value)} placeholder="如 prod-pg-backup" />
        </Field>
        <Field label="备份类型">
          <Select value={kind} onChange={(v) => onKindChange(v as BackupKind)}>
            {(['database', 'volume', 'dir', 'panel'] as BackupKind[]).map((k) => <option key={k} value={k}>{kindLabels[k]}</option>)}
          </Select>
        </Field>
        <Field label="归档格式">
          <Select value={format} onChange={(v) => setFormat(v)}>
            {kind === 'database' ? (
              <>
                <option value="pgdump">{formatLabels.pgdump}</option>
                <option value="sql">{formatLabels.sql}</option>
                <option value="archive">{formatLabels.archive}</option>
                <option value="rdb">{formatLabels.rdb}</option>
              </>
            ) : kind === 'panel' ? (
              <option value="sqlite">{formatLabels.sqlite}</option>
            ) : (
              <option value="tar">{formatLabels.tar}</option>
            )}
          </Select>
        </Field>
      </div>
      <label className="check">
        <input type="checkbox" checked={openRestore} onChange={(e) => setOpenRestore(e.target.checked)} />
        上传完成后立即打开恢复弹窗
      </label>
      {progress != null && (
        <div className="col" style={{ gap: 4 }}>
          <div className="progress"><div style={{ width: progress + '%' }} /></div>
          <span className="label">正在流式上传并计算校验和… {progress}%</span>
        </div>
      )}
      <ErrorBox error={err} />
    </Modal>
  )
}

type ScannedItem = {
  objectKey: string
  size: number
  modTime: number
  recognized: boolean
  imported: boolean
  backupId?: number
  kind: BackupKind | ''
  format: string
  engineHint?: string
  volumeHint?: string
  encrypted: boolean
  compression: string
  label: string
  startedAt: number
}

export function ScanStorageModal({ onClose, onImported }: { onClose: () => void; onImported: (backup: any | null, openRestore: boolean) => void }) {
  const { data: storages } = useStorages()
  const toast = useToast()
  const [storageId, setStorageId] = useState(0)
  const [scanning, setScanning] = useState(false)
  const [importing, setImporting] = useState(false)
  const [items, setItems] = useState<ScannedItem[] | null>(null)
  const [truncated, setTruncated] = useState(false)
  const [onlyNew, setOnlyNew] = useState(true)
  const [onlyRecognized, setOnlyRecognized] = useState(true)
  const [q, setQ] = useState('')
  const [selected, setSelected] = useState<Record<string, boolean>>({})
  const [overrides, setOverrides] = useState<Record<string, { kind: BackupKind; format: string }>>({})
  const [err, setErr] = useState<unknown>()

  const sid = storageId || storages?.[0]?.id || 0

  useEffect(() => {
    setItems(null)
    setSelected({})
    setOverrides({})
    setErr(undefined)
  }, [sid])

  const scan = async () => {
    if (!sid) return
    setScanning(true)
    setErr(undefined)
    try {
      const res = await post<{ items: ScannedItem[]; truncated: boolean }>(`/storage/${sid}/scan`)
      setItems(res.items || [])
      setTruncated(!!res.truncated)
      setSelected({})
      setOverrides({})
    } catch (e) {
      setErr(e)
    } finally {
      setScanning(false)
    }
  }

  const effectiveOf = (it: ScannedItem): { kind: BackupKind; format: string } => {
    if (overrides[it.objectKey]) return overrides[it.objectKey]
    const k: BackupKind = (it.kind || 'database') as BackupKind
    const f = it.format || (k === 'panel' ? 'sqlite' : k === 'database' ? 'sql' : 'tar')
    return { kind: k, format: f }
  }

  const updateOverride = (it: ScannedItem, patch: Partial<{ kind: BackupKind; format: string }>) => {
    const cur = effectiveOf(it)
    const nextKind = patch.kind ?? cur.kind
    let nextFmt = patch.format ?? cur.format
    if (patch.kind) {
      if (nextKind === 'volume' || nextKind === 'dir') nextFmt = 'tar'
      else if (nextKind === 'panel') nextFmt = 'sqlite'
      else if (!['pgdump', 'sql', 'archive', 'rdb'].includes(nextFmt)) nextFmt = 'sql'
    }
    setOverrides((prev) => ({ ...prev, [it.objectKey]: { kind: nextKind, format: nextFmt } }))
  }

  const filtered = (items || []).filter((it) => {
    if (onlyNew && it.imported) return false
    if (onlyRecognized && !it.recognized) return false
    if (q && !it.objectKey.toLowerCase().includes(q.toLowerCase()) && !it.label.toLowerCase().includes(q.toLowerCase())) return false
    return true
  })

  const selectableKeys = filtered.filter((it) => !it.imported).map((it) => it.objectKey)
  const selectedItems = (items || []).filter((it) => !it.imported && selected[it.objectKey])
  const allSelected = selectableKeys.length > 0 && selectableKeys.every((k) => selected[k])

  const toggleAll = () => {
    if (allSelected) {
      const next = { ...selected }
      for (const k of selectableKeys) delete next[k]
      setSelected(next)
    } else {
      const next = { ...selected }
      for (const k of selectableKeys) next[k] = true
      setSelected(next)
    }
  }

  const importList = async (list: ScannedItem[], openRestore: boolean) => {
    if (!sid || list.length === 0) return
    setImporting(true)
    setErr(undefined)
    try {
      const payload = list.map((it) => {
        const eff = effectiveOf(it)
        return {
          objectKey: it.objectKey,
          kind: eff.kind,
          format: eff.format,
          label: it.label,
          volumeHint: it.volumeHint || '',
          encrypted: it.encrypted,
          size: it.size,
          startedAt: it.startedAt,
        }
      })
      const res = await post<{ imported: any[]; skipped: number }>('/backups/import/storage', { storageId: sid, items: payload })
      toast.ok(`已导入 ${res.imported.length} 条备份记录${res.skipped > 0 ? `（跳过 ${res.skipped} 条已存在）` : ''}`)
      onImported(openRestore ? res.imported[0] || null : null, openRestore && res.imported.length > 0)
      onClose()
    } catch (e) {
      setErr(e)
    } finally {
      setImporting(false)
    }
  }

  return (
    <Modal
      title="从存储位置扫描导入历史备份"
      wide
      onClose={onClose}
      footer={
        <>
          <button className="btn" disabled={importing} onClick={onClose}>关闭</button>
          <span className="spacer" />
          <button className="btn btn-primary" disabled={importing || selectedItems.length === 0} onClick={() => importList(selectedItems, false)}>
            {importing ? '导入中…' : `导入选中（${selectedItems.length}）`}
          </button>
        </>
      }
    >
      <div className="row wrap" style={{ gap: 10, alignItems: 'flex-end' }}>
        <div className="grow" style={{ minWidth: 220 }}>
          <Field label="存储位置">
            <Select value={sid || ''} onChange={(v) => setStorageId(Number(v))}>
              {(storages || []).map((s) => <option key={s.id} value={s.id}>{s.name}（{storageKindName[s.kind] || s.kind}）</option>)}
            </Select>
          </Field>
        </div>
        <button className="btn btn-primary" disabled={!sid || scanning} onClick={scan}>
          <Icon name="search" size={16} />{scanning ? '扫描中…' : items ? '重新扫描' : '开始扫描'}
        </button>
      </div>

      {truncated && (
        <div className="alert warn"><Icon name="alert" />该存储对象超过 5000 个（或目录层级超过 6 层），列表已截断。</div>
      )}

      {items && (
        <>
          <div className="row wrap" style={{ gap: 10 }}>
            <input className="input filter-input" placeholder="筛选路径或标签" value={q} onChange={(e) => setQ(e.target.value)} />
            <label className="check"><input type="checkbox" checked={onlyNew} onChange={(e) => setOnlyNew(e.target.checked)} />仅未导入</label>
            <label className="check"><input type="checkbox" checked={onlyRecognized} onChange={(e) => setOnlyRecognized(e.target.checked)} />仅已识别备份</label>
            <span className="label">共 {filtered.length} / {items.length} 个对象</span>
          </div>

          {filtered.length === 0 ? (
            <Empty>当前筛选条件下没有可导入的对象。</Empty>
          ) : (
            <div style={{ maxHeight: '52vh', overflowY: 'auto' }}>
              <ResponsiveTable
                table={
                  <div className="table-wrap">
                    <table>
                      <thead>
                        <tr>
                          <th style={{ width: 32 }}><input type="checkbox" checked={allSelected} onChange={toggleAll} aria-label="全选" /></th>
                          <th>对象路径</th>
                          <th>推断类型 / 格式</th>
                          <th>大小</th>
                          <th>时间</th>
                          <th></th>
                        </tr>
                      </thead>
                      <tbody>
                        {filtered.map((it) => {
                          const eff = effectiveOf(it)
                          return (
                            <tr key={it.objectKey}>
                              <td>
                                <input
                                  type="checkbox"
                                  disabled={it.imported}
                                  checked={!!selected[it.objectKey]}
                                  onChange={(e) => setSelected((prev) => ({ ...prev, [it.objectKey]: e.target.checked }))}
                                />
                              </td>
                              <td>
                                <div className="mono" style={{ fontSize: 12, overflowWrap: 'anywhere' }}>{it.objectKey}</div>
                                <div className="row wrap" style={{ gap: 6, marginTop: 4 }}>
                                  {it.imported && <span className="badge ok">已导入</span>}
                                  {!it.recognized && <span className="badge warn">未识别后缀</span>}
                                  {it.encrypted && <span className="badge off"><Icon name="lock" size={11} />age 加密</span>}
                                  {it.compression !== 'none' && <span className="tag mono">{it.compression}</span>}
                                </div>
                              </td>
                              <td>
                                <div className="row wrap" style={{ gap: 6 }}>
                                  <Select value={eff.kind} disabled={it.imported} onChange={(v) => updateOverride(it, { kind: v as BackupKind })}>
                                    {(['database', 'volume', 'dir', 'panel'] as BackupKind[]).map((k) => <option key={k} value={k}>{kindLabels[k]}</option>)}
                                  </Select>
                                  {eff.kind === 'database' && (
                                    <Select value={eff.format} disabled={it.imported} onChange={(v) => updateOverride(it, { format: v })}>
                                      <option value="pgdump">pgdump</option>
                                      <option value="sql">sql</option>
                                      <option value="archive">archive</option>
                                      <option value="rdb">rdb</option>
                                    </Select>
                                  )}
                                </div>
                              </td>
                              <td className="nowrap">{it.size >= 0 ? bytes(it.size) : '—'}</td>
                              <td className="nowrap muted">{ago(it.startedAt)}</td>
                              <td>
                                {!it.imported && (
                                  <button className="btn btn-sm" disabled={importing} onClick={() => importList([it], true)}>导入并恢复</button>
                                )}
                              </td>
                            </tr>
                          )
                        })}
                      </tbody>
                    </table>
                  </div>
                }
                cards={filtered.map((it) => {
                  const eff = effectiveOf(it)
                  return (
                    <MobileCard
                      key={it.objectKey}
                      tight
                      title={<span className="mono" style={{ fontSize: 12 }}>{it.objectKey}</span>}
                      badges={
                        <>
                          {it.imported && <span className="badge ok">已导入</span>}
                          {!it.recognized && <span className="badge warn">未识别</span>}
                          {it.encrypted && <span className="badge off"><Icon name="lock" size={11} />加密</span>}
                        </>
                      }
                      meta={[
                        { label: '类型', value: `${kindLabels[eff.kind]} / ${eff.format}` },
                        { label: '大小', value: it.size >= 0 ? bytes(it.size) : '—' },
                        { label: '时间', value: ago(it.startedAt) },
                      ]}
                      actions={
                        !it.imported ? (
                          <>
                            <label className="check">
                              <input
                                type="checkbox"
                                checked={!!selected[it.objectKey]}
                                onChange={(e) => setSelected((prev) => ({ ...prev, [it.objectKey]: e.target.checked }))}
                              />
                              选中
                            </label>
                            <button className="btn btn-sm btn-primary" disabled={importing} onClick={() => importList([it], true)}>导入并恢复</button>
                          </>
                        ) : undefined
                      }
                    />
                  )
                })}
              />
            </div>
          )}
        </>
      )}
      {scanning && !items && <Loading />}
      <ErrorBox error={err} />
    </Modal>
  )
}
