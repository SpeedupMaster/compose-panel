import { useEffect, useState } from 'react'
import { bytes, dateTime, del, get } from '../api'
import { useCan } from '../auth'
import {
  archiveReasonLabel,
  archiveTargets,
  filterArchivesByTarget,
  formatArchiveOption,
  LogArchiveItem,
  nextArchiveAfterDelete,
  resolveActiveArchive,
  resolveArchiveTarget,
  stripDockerTimestamp,
} from './logStream'
import { Confirm, Empty, ErrorBox, Icon, Loading, LogView, Modal, Select, useToast } from './ui'

export function LogArchivesModal({
  title,
  archives,
  showProject = false,
  onClose,
  onChanged,
}: {
  title: string
  archives: LogArchiveItem[]
  showProject?: boolean
  onClose: () => void
  onChanged: () => void
}) {
  const can = useCan()
  const toast = useToast()
  const [deletedIds, setDeletedIds] = useState<string[]>([])
  const visibleArchives = deletedIds.length > 0 ? archives.filter((a) => !deletedIds.includes(a.id)) : archives
  const targets = archiveTargets(visibleArchives, showProject)
  const [target, setTarget] = useState('')
  const effectiveTarget = resolveArchiveTarget(targets, target)
  const filtered = filterArchivesByTarget(visibleArchives, effectiveTarget, showProject)
  const [selectedId, setSelectedId] = useState(() => filtered[0]?.id || '')
  const [tail, setTail] = useState('500')
  const [filter, setFilter] = useState('')
  const [ts, setTs] = useState(false)
  const [lines, setLines] = useState<string[]>([])
  const [meta, setMeta] = useState<LogArchiveItem | null>(null)
  const [totalLines, setTotalLines] = useState(0)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<unknown>(null)
  const [deleting, setDeleting] = useState<LogArchiveItem | null>(null)

  // Reset stale target filter if its last archive was deleted.
  useEffect(() => {
    if (target && effectiveTarget === '') {
      setTarget('')
    }
  }, [target, effectiveTarget])

  // Keep selectedId valid when target filter or archive list changes.
  useEffect(() => {
    if (filtered.length === 0) {
      if (selectedId !== '') setSelectedId('')
      return
    }
    if (!filtered.some((a) => a.id === selectedId)) {
      setSelectedId(filtered[0].id)
    }
  }, [filtered, selectedId])

  useEffect(() => {
    if (!selectedId) {
      setLines([])
      setMeta(null)
      setTotalLines(0)
      setError(null)
      return
    }
    let cancelled = false
    setLoading(true)
    setError(null)
    setMeta(null)
    setTotalLines(0)
    setLines([])
    get<{ archive: LogArchiveItem; lines: string[]; totalLines: number }>(
      `/log-archives/${encodeURIComponent(selectedId)}?tail=${encodeURIComponent(tail)}`,
    )
      .then((res) => {
        if (cancelled) return
        setMeta(res.archive)
        setTotalLines(res.totalLines)
        setLines(res.lines || [])
      })
      .catch((err) => {
        if (cancelled) return
        setError(err)
        setMeta(null)
        setLines([])
      })
      .finally(() => {
        if (!cancelled) setLoading(false)
      })
    return () => {
      cancelled = true
    }
  }, [selectedId, tail])

  const active = resolveActiveArchive(meta, filtered, selectedId)
  const shown = ts ? lines : lines.map(stripDockerTimestamp)

  return (
    <Modal title={title} wide onClose={onClose}>
      {visibleArchives.length === 0 ? (
        <Empty>暂无历史日志归档。</Empty>
      ) : (
        <div className="col" style={{ gap: 12 }}>
          <div className="row wrap" style={{ justifyContent: 'space-between', gap: 8 }}>
            <div className="row wrap" style={{ gap: 8 }}>
              {targets.length > 1 && (
                <Select
                  className="filter-select"
                  style={{ width: 170 }}
                  aria-label="筛选服务或容器"
                  value={effectiveTarget}
                  onChange={(v) => setTarget(v)}
                >
                  <option value="">全部容器（{visibleArchives.length}）</option>
                  {targets.map((t) => (
                    <option key={t} value={t}>
                      {t}
                    </option>
                  ))}
                </Select>
              )}
              <Select
                className="filter-select"
                style={{ width: 280 }}
                aria-label="选择历史归档"
                value={selectedId}
                onChange={(v) => setSelectedId(v)}
              >
                {filtered.map((a) => (
                  <option key={a.id} value={a.id}>
                    {formatArchiveOption(a, effectiveTarget === '', showProject)}
                  </option>
                ))}
              </Select>
              <input
                className="input filter-input"
                placeholder="筛选"
                aria-label="筛选归档日志"
                value={filter}
                onChange={(e) => setFilter(e.target.value)}
              />
              <Select className="filter-select" style={{ width: 132 }} aria-label="行数" value={tail} onChange={(v) => setTail(v)}>
                {['200', '500', '2000', '10000', 'all'].map((t) => (
                  <option key={t} value={t}>
                    {t === 'all' ? '全部' : `最近 ${t} 行`}
                  </option>
                ))}
              </Select>
              <label className="check">
                <input type="checkbox" checked={ts} onChange={(e) => setTs(e.target.checked)} />
                时间戳
              </label>
            </div>
            {active && (
              <div className="row wrap" style={{ gap: 8 }}>
                {can.operator && (
                  <button className="btn btn-sm btn-danger" onClick={() => setDeleting(active)}>
                    <Icon name="trash" size={14} />删除
                  </button>
                )}
                <a className="btn btn-sm" href={`/api/log-archives/${encodeURIComponent(active.id)}?download=1`}>
                  <Icon name="download" size={16} />下载
                </a>
              </div>
            )}
          </div>
          {active && (
            <div
              className="row wrap"
              style={{
                padding: '8px 12px',
                border: '1px solid var(--line)',
                borderRadius: 8,
                background: 'var(--bg)',
                fontSize: 13,
                gap: '6px 14px',
              }}
            >
              <span className="badge off">{archiveReasonLabel(active.reason)}</span>
              <span className="mono">{active.containerName}</span>
              <span className="muted">归档于 {dateTime(active.archivedAt)}</span>
              <span className="mono muted">容器 {active.containerId.slice(0, 12)}</span>
              {active.image && <span className="mono muted">{active.image}</span>}
              <span className="muted">
                共 {totalLines || active.lineCount} 行 · {bytes(active.sizeBytes)}
              </span>
              {active.truncated && <span className="badge warn">已截断（仅保留最近 20,000 行）</span>}
            </div>
          )}
          {error ? (
            <ErrorBox error={error} />
          ) : loading ? (
            <Loading />
          ) : (
            <div style={{ borderRadius: 8, overflow: 'hidden', border: '1px solid var(--line)' }}>
              <LogView lines={shown} height={460} follow={false} filter={filter} />
            </div>
          )}
        </div>
      )}
      {deleting && (
        <Confirm
          title="删除历史日志归档"
          danger
          confirmText="删除"
          onClose={() => setDeleting(null)}
          onConfirm={async () => {
            const deletedId = deleting.id
            await del(`/log-archives/${encodeURIComponent(deletedId)}`)
            toast.ok('已删除历史日志归档')
            const remainingVisible = visibleArchives.filter((a) => a.id !== deletedId)
            const nextTargets = archiveTargets(remainingVisible, showProject)
            const nextTarget = resolveArchiveTarget(nextTargets, effectiveTarget)
            const nextFiltered = filterArchivesByTarget(remainingVisible, nextTarget, showProject)
            setDeletedIds((prev) => [...prev, deletedId])
            setTarget(nextTarget)
            setSelectedId(nextArchiveAfterDelete(nextFiltered, deletedId, selectedId) || nextFiltered[0]?.id || '')
            onChanged()
          }}
        >
          确定删除 <code className="mono">{deleting.containerName}</code> 在 {dateTime(deleting.archivedAt)} 的历史日志归档？删除后无法恢复。
        </Confirm>
      )}
    </Modal>
  )
}
