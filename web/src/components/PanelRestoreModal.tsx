import { useEffect, useState } from 'react'
import { ago, bytes, get, post, uploadForm } from '../api'
import { ErrorBox, Field, FilePicker, Icon, Loading, Modal, Select, useLoad } from './ui'

export type PanelRestoreSummary = {
  users: number
  projects: number
  databases: number
  sites: number
  storages: number
  policies: number
  backups: number
  keyMatched: boolean
  needMasterKey: boolean
}

export function PanelSummaryGrid({ summary }: { summary: PanelRestoreSummary }) {
  return (
    <div className="col" style={{ gap: 10 }}>
      {summary.keyMatched ? (
        <div className="alert ok"><Icon name="check" />校验通过：SQLite 完整性正常，主密钥可正常解密备份中的凭据。</div>
      ) : (
        <div className="alert warn"><Icon name="alert" />检测到该备份来自其他服务器或使用了不同的 <code>master.key</code>，请在下方输入原服务器的 64 位 <code>master.key</code> 后重新检查。</div>
      )}
      <dl className="kv">
        <dt>用户</dt><dd>{summary.users}</dd>
        <dt>应用项目</dt><dd>{summary.projects}</dd>
        <dt>数据库实例</dt><dd>{summary.databases}</dd>
        <dt>反代站点</dt><dd>{summary.sites}</dd>
        <dt>存储位置</dt><dd>{summary.storages}</dd>
        <dt>备份策略 / 记录</dt><dd>{summary.policies} 策略 · {summary.backups} 条记录</dd>
      </dl>
    </div>
  )
}

export function RestartWaitOverlay({ title = '正在恢复面板并重启…' }: { title?: string }) {
  const [elapsed, setElapsed] = useState(0)
  const [timedOut, setTimedOut] = useState(false)

  useEffect(() => {
    let stopped = false
    let sawDown = false
    const started = Date.now()

    const tick = async () => {
      if (stopped) return
      const sec = Math.floor((Date.now() - started) / 1000)
      setElapsed(sec)
      if (sec >= 30) {
        setTimedOut(true)
        return
      }
      try {
        const res = await fetch('/api/setup/status', { cache: 'no-store' })
        if (res.ok) {
          const data = await res.json().catch(() => null)
          // Wait at least 1.2s (or after seeing a restart blip) so we don't redirect before reload finishes.
          if ((sawDown || Date.now() - started > 1500) && data && data.needed === false) {
            stopped = true
            window.location.href = '/login?restored=1'
            return
          }
        } else {
          sawDown = true
        }
      } catch {
        sawDown = true
      }
      if (!stopped) window.setTimeout(tick, 1000)
    }

    const t = window.setTimeout(tick, 900)
    return () => {
      stopped = true
      window.clearTimeout(t)
    }
  }, [])

  return (
    <Modal title={title} onClose={() => {}}>
      <div className="col" style={{ gap: 12 }}>
        {!timedOut ? (
          <>
            <div className="alert ok"><Icon name="restart" />面板正在应用新数据库并重新加载服务（已等待 {elapsed} 秒），完成后将自动跳转到登录页。</div>
            <p className="label" style={{ margin: 0 }}>所有现有登录会话会被清空，重启完成后请使用备份中的原管理员账号重新登录。</p>
          </>
        ) : (
          <>
            <div className="alert warn"><Icon name="alert" />等待自动重连超时。若面板绑定了新域名或端口已变更，请直接访问新地址或手动刷新页面。</div>
            <div><button className="btn btn-primary" onClick={() => { window.location.href = '/login' }}>前往登录页</button></div>
          </>
        )}
      </div>
    </Modal>
  )
}

type LocalPanelSnap = { name: string; size: number; modTime: number }

export function PanelRestoreModal({ backup, initialSnapshot, onClose }: { backup?: any; initialSnapshot?: string; onClose: () => void }) {
  const fromRecord = !!backup
  const [sourceTab, setSourceTab] = useState<'snapshot' | 'upload'>(initialSnapshot ? 'snapshot' : 'snapshot')
  const { data: snaps } = useLoad(() => (fromRecord ? Promise.resolve(null) : get<LocalPanelSnap[]>('/system/panel-backups')), [fromRecord])
  const [snapshot, setSnapshot] = useState(initialSnapshot || '')
  const [file, setFile] = useState<File | null>(null)
  const [ageIdentity, setAgeIdentity] = useState('')
  const [masterKey, setMasterKey] = useState('')
  const [confirm, setConfirm] = useState('')
  const [summary, setSummary] = useState<PanelRestoreSummary | null>(null)
  const [busy, setBusy] = useState(false)
  const [progress, setProgress] = useState<number | null>(null)
  const [err, setErr] = useState<unknown>()
  const [restarting, setRestarting] = useState(false)

  useEffect(() => {
    if (!fromRecord && !snapshot && snaps && snaps.length > 0) {
      setSnapshot(snaps[0].name)
    }
  }, [fromRecord, snapshot, snaps])

  const resetCheck = () => {
    setSummary(null)
    setErr(undefined)
  }

  const runRequest = async (dryRun: boolean) => {
    setBusy(true)
    setErr(undefined)
    try {
      let res: any
      if (fromRecord) {
        res = await post(`/backups/${backup.id}/restore`, {
          kind: 'panel',
          dryRun,
          ageIdentity: ageIdentity.trim() || undefined,
          masterKey: masterKey.trim() || undefined,
          confirm: dryRun ? undefined : confirm.trim(),
        })
      } else if (sourceTab === 'snapshot') {
        if (!snapshot) throw new Error('请选择一个本地面板快照')
        res = await post('/system/panel-restore', {
          snapshot,
          dryRun,
          ageIdentity: ageIdentity.trim() || undefined,
          masterKey: masterKey.trim() || undefined,
          confirm: dryRun ? undefined : confirm.trim(),
        })
      } else {
        if (!file) throw new Error('请选择面板备份文件')
        const fd = new FormData()
        fd.append('file', file)
        if (ageIdentity.trim()) fd.append('ageIdentity', ageIdentity.trim())
        if (masterKey.trim()) fd.append('masterKey', masterKey.trim())
        if (dryRun) fd.append('dryRun', '1')
        else fd.append('confirm', confirm.trim())
        setProgress(0)
        res = await uploadForm('/system/panel-restore', fd, setProgress)
      }
      if (dryRun) {
        setSummary(res.summary)
      } else if (res?.restarting) {
        setRestarting(true)
      }
    } catch (e) {
      setErr(e)
    } finally {
      setBusy(false)
      setProgress(null)
    }
  }

  if (restarting) {
    return <RestartWaitOverlay />
  }

  const canCheck = fromRecord || (sourceTab === 'snapshot' ? !!snapshot : !!file)
  const canApply = !!summary?.keyMatched && (confirm.trim() === 'RESTORE' || confirm.trim() === '恢复面板')

  return (
    <Modal
      title="恢复面板数据（panel.db）"
      wide
      onClose={onClose}
      footer={
        <>
          <button className="btn" disabled={busy} onClick={onClose}>取消</button>
          <span className="spacer" />
          <button className="btn" disabled={busy || !canCheck} onClick={() => runRequest(true)}>
            {busy && !summary?.keyMatched ? '检查中…' : summary ? '重新检查' : '检查备份'}
          </button>
          {summary?.keyMatched && (
            <button className="btn btn-danger solid" disabled={busy || !canApply} onClick={() => runRequest(false)}>
              {busy ? '正在应用…' : '确认覆盖并重启面板'}
            </button>
          )}
        </>
      }
    >
      {fromRecord ? (
        <dl className="kv">
          <dt>备份对象</dt><dd className="mono" style={{ fontSize: 12 }}>{backup.objectKey}</dd>
          <dt>时间 / 大小</dt><dd>{ago(backup.startedAt)} · {bytes(backup.size)}{backup.encrypted ? ' · 已加密' : ''}</dd>
        </dl>
      ) : (
        <>
          <div className="seg" role="group" aria-label="恢复来源">
            <button type="button" className={sourceTab === 'snapshot' ? 'on' : ''} onClick={() => { setSourceTab('snapshot'); resetCheck() }}>本地面板快照</button>
            <button type="button" className={sourceTab === 'upload' ? 'on' : ''} onClick={() => { setSourceTab('upload'); resetCheck() }}>上传备份文件</button>
          </div>
          {sourceTab === 'snapshot' ? (
            !snaps ? <Loading /> : snaps.length === 0 ? (
              <div className="muted">本地 <code>_panel</code> 目录下暂无面板快照，请切换到「上传备份文件」或先执行一次面板备份。</div>
            ) : (
              <Field label="选择本地快照">
                <Select value={snapshot} onChange={(v) => { setSnapshot(v); resetCheck() }}>
                  {snaps.map((s) => <option key={s.name} value={s.name}>{s.name}（{bytes(s.size)} · {ago(s.modTime)}）</option>)}
                </Select>
              </Field>
            )
          ) : (
            <Field label="备份文件" div>
              <FilePicker
                file={file}
                onChange={(f) => { setFile(f); resetCheck() }}
                accept=".db,.sqlite,.sqlite3,.zst,.age,.gz"
                disabled={busy}
                hint="支持 .db / .sqlite / .sqlite.zst / .sqlite.zst.age"
              />
            </Field>
          )}
        </>
      )}

      {(fromRecord ? backup.encrypted : sourceTab === 'upload') && (
        <Field label="临时 age 解密私钥（可选）" hint="若该文件由其他面板加密且尚未导入历史密钥环，可在此临时粘贴 AGE-SECRET-KEY-1…（不保存）">
          <input className="input mono" type="password" value={ageIdentity} onChange={(e) => { setAgeIdentity(e.target.value); resetCheck() }} placeholder="AGE-SECRET-KEY-1…" autoComplete="off" />
        </Field>
      )}

      <Field label="原服务器 master.key（跨机迁移时填写，同机可留空）" hint="64 位十六进制主密钥；留空默认使用本机当前 master.key 验证">
        <input className="input mono" type="password" value={masterKey} onChange={(e) => { setMasterKey(e.target.value); resetCheck() }} placeholder="留空复用本机 master.key" autoComplete="off" />
      </Field>

      {progress != null && (
        <div className="col" style={{ gap: 4 }}>
          <div className="progress"><div style={{ width: progress + '%' }} /></div>
          <span className="label">上传中 {progress}%</span>
        </div>
      )}

      {summary && <PanelSummaryGrid summary={summary} />}

      {summary?.keyMatched && (
        <>
          <div className="alert err">
            <Icon name="alert" />
            <div>覆盖恢复会先自动备份当前 <code>panel.db</code>（及旧 <code>master.key</code>），随后替换数据库并平滑重启面板。所有现有登录会话会被注销；如有运行中的后台任务请先等待结束。</div>
          </div>
          <Field label={<>输入 <code className="mono">RESTORE</code> 确认</>}>
            <input className="input mono" value={confirm} onChange={(e) => setConfirm(e.target.value)} placeholder="RESTORE" autoComplete="off" />
          </Field>
        </>
      )}

      <ErrorBox error={err} />
    </Modal>
  )
}
