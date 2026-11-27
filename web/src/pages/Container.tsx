import { useEffect, useRef, useState } from 'react'
import { Link, useNavigate, useParams, useSearchParams } from 'react-router-dom'
import type { Terminal } from '@xterm/xterm'
import { bytes, dateTime, del, get, post, streamLines } from '../api'
import { useCan } from '../auth'
import { MetricsPanel } from '../components/charts'
import { FileBrowser } from '../components/files'
import {
  advanceLogCursor,
  archiveReasonLabel,
  formatArchiveOption,
  formatLogCursor,
  LiveStreamSnapshot,
  LogArchiveItem,
  LogCursor,
  resolveActiveArchive,
  shouldResumeLiveLogs,
  stripDockerTimestamp,
} from '../components/logStream'
import { MobileCard, ResponsiveTable } from '../components/ResponsiveList'
import { Card, Confirm, ErrorBox, Icon, Loading, LogView, Meter, Page, Select, Status, Tabs, useAction, useLoad, useToast } from '../components/ui'

type Tab = 'logs' | 'terminal' | 'files' | 'metrics' | 'info'

export default function ContainerPage() {
  const { id } = useParams()
  const [params, setParams] = useSearchParams()
  const tab = (params.get('tab') as Tab) || 'logs'
  const { data: c, error, reload } = useLoad(() => get<any>(`/containers/${id}`), [id], 10000)
  const can = useCan()
  const nav = useNavigate()
  const { busy, run } = useAction()
  const [removing, setRemoving] = useState(false)
  if (!c) return <Page title="容器">{error ? <ErrorBox error={error} /> : <Loading />}</Page>
  const act = async (a: string) => {
    await run(a, () => post(`/containers/${id}/${a}`), '操作已完成')
    reload()
  }
  const state = c.health === 'unhealthy' ? 'unhealthy' : c.state
  return (
    <Page
      title={c.service || c.name}
      crumbs={c.project ? <><Link to="/projects">项目</Link> / {c.project} / {c.name}</> : <><Link to="/docker">Docker 资源</Link> / {c.name}</>}
      sub={<span className="row wrap"><Status s={state} /><span className="mono" style={{ overflowWrap: 'anywhere' }}>{c.image}</span>{c.restartCount > 0 && <span className="badge warn">重启 {c.restartCount} 次</span>}{c.oomKilled && <span className="badge err">OOM</span>}</span>}
      actions={
        can.operator && (
          <>
            {c.running ? (
              <>
                <button className="btn" disabled={!!busy} onClick={() => act('restart')}><Icon name="restart" />重启</button>
                <button className="btn" disabled={!!busy} onClick={() => act('stop')}><Icon name="stop" />停止</button>
              </>
            ) : (
              <button className="btn btn-primary" disabled={!!busy} onClick={() => act('start')}><Icon name="play" />启动</button>
            )}
            {can.admin && <button className="btn btn-danger" onClick={() => setRemoving(true)}>删除</button>}
          </>
        )
      }
    >
      <Tabs
        tabs={[['logs', '日志'], ...(can.operator ? [['terminal', '终端'] as [Tab, string], ['files', '文件'] as [Tab, string]] : []), ['metrics', '监控'], ['info', '详情与资源']]}
        value={tab}
        onChange={(t) => setParams({ tab: t }, { replace: true })}
      />
      {tab === 'logs' && (
        <Logs
          id={id!}
          running={c.running}
          startedAt={c.startedAt || ''}
          restartCount={c.restartCount || 0}
          canOperate={can.operator}
          onStreamEnd={reload}
        />
      )}
      {tab === 'terminal' && (c.running ? <Term id={id!} /> : <div className="alert warn">容器未运行，无法打开终端。</div>)}
      {tab === 'files' && <FileBrowser base={`/containers/${id}/fs`} canWrite={can.operator} note="上传会覆盖同名文件；文件变更记录到审计日志。" />}
      {tab === 'metrics' && <MetricsPanel url={`/containers/${id}/metrics`} />}
      {tab === 'info' && <Info c={c} id={id!} />}
      {removing && (
        <Confirm title={`删除容器 ${c.name}`} danger confirmText="删除" onClose={() => setRemoving(false)} onConfirm={async () => { await post(`/containers/${id}/remove`); nav(-1) }}>
          强制删除这个容器。如果它属于 Compose 项目，下次部署会重新创建。
        </Confirm>
      )}
    </Page>
  )
}

function Logs({
  id,
  running,
  startedAt,
  restartCount,
  canOperate,
  onStreamEnd,
}: {
  id: string
  running: boolean
  startedAt: string
  restartCount: number
  canOperate: boolean
  onStreamEnd: () => void
}) {
  const [lines, setLines] = useState<string[]>([])
  const [tail, setTail] = useState('500')
  const [follow, setFollow] = useState(true)
  const [paused, setPaused] = useState(false)
  const [filter, setFilter] = useState('')
  const [ts, setTs] = useState(false)
  const [ended, setEnded] = useState(false)
  const [source, setSource] = useState('live')
  const [resumeEpoch, setResumeEpoch] = useState(0)
  const [archiveMeta, setArchiveMeta] = useState<LogArchiveItem | null>(null)
  const [archiveTotal, setArchiveTotal] = useState(0)
  const [archiveLoading, setArchiveLoading] = useState(false)
  const [archiveError, setArchiveError] = useState<unknown>(null)
  const [deletingArchive, setDeletingArchive] = useState<LogArchiveItem | null>(null)
  const toast = useToast()

  const { data: archives, reload: reloadArchives } = useLoad(
    () => get<LogArchiveItem[]>(`/containers/${id}/log-archives`),
    [id],
  )

  const bufRef = useRef<string[]>([])
  const cursorRef = useRef<LogCursor | null>(null)
  const prevStreamKeyRef = useRef('')
  const streamSnapRef = useRef<LiveStreamSnapshot | null>(null)
  const pausedRef = useRef(false)
  pausedRef.current = paused
  const runningRef = useRef(running)
  runningRef.current = running
  const startedAtRef = useRef(startedAt)
  startedAtRef.current = startedAt
  const restartCountRef = useRef(restartCount)
  restartCountRef.current = restartCount
  const onStreamEndRef = useRef(onStreamEnd)
  onStreamEndRef.current = onStreamEnd

  // Reset selected source when navigating to a different container.
  useEffect(() => {
    setSource('live')
  }, [id])

  // Live SSE streaming with cursor-based incremental reconnect and seamless restart resume.
  useEffect(() => {
    if (source !== 'live') return
    const streamKey = `${id}|${tail}|${follow}`
    const isResume = prevStreamKeyRef.current === streamKey && resumeEpoch > 0
    prevStreamKeyRef.current = streamKey

    if (!isResume) {
      bufRef.current = []
      cursorRef.current = null
      setLines([])
    }
    setEnded(false)
    setArchiveMeta(null)
    setArchiveError(null)
    streamSnapRef.current = {
      running: runningRef.current,
      startedAt: startedAtRef.current,
      restartCount: restartCountRef.current,
    }

    let raf = 0
    const retryTimers: number[] = []
    const after = isResume ? formatLogCursor(cursorRef.current) : ''
    const path = after
      ? `/containers/${id}/logs?tail=all&follow=${follow ? 1 : 0}&after=${encodeURIComponent(after)}`
      : `/containers/${id}/logs?tail=${tail}&follow=${follow && runningRef.current ? 1 : 0}`

    const close = streamLines(
      path,
      (l) => {
        cursorRef.current = advanceLogCursor(cursorRef.current, l)
        const buf = bufRef.current
        buf.push(l)
        if (buf.length > 20000) buf.splice(0, buf.length - 20000)
        if (!raf && !pausedRef.current) {
          raf = requestAnimationFrame(() => {
            raf = 0
            setLines([...bufRef.current])
          })
        }
      },
      () => {
        setEnded(true)
        if (!pausedRef.current) setLines([...bufRef.current])
        if (follow) {
          onStreamEndRef.current?.()
          retryTimers.push(window.setTimeout(() => onStreamEndRef.current?.(), 1500))
        }
      },
      undefined,
      { resetOnReconnect: false },
    )
    return () => {
      close()
      if (raf) cancelAnimationFrame(raf)
      retryTimers.forEach((t) => clearTimeout(t))
    }
  }, [id, tail, follow, source, resumeEpoch])

  // Detect container start or fast in-place restart after stream EOF and resume without clearing.
  useEffect(() => {
    if (source !== 'live') return
    if (shouldResumeLiveLogs(streamSnapRef.current, { running, startedAt, restartCount }, ended, follow)) {
      setResumeEpoch((n) => n + 1)
    }
  }, [running, startedAt, restartCount, ended, follow, source])

  // Immediately flush buffered lines when unpausing.
  useEffect(() => {
    if (source === 'live' && !paused) {
      setLines([...bufRef.current])
    }
  }, [paused, source])

  // Load selected historical log archive.
  useEffect(() => {
    if (source === 'live') return
    prevStreamKeyRef.current = ''
    let cancelled = false
    setArchiveLoading(true)
    setArchiveError(null)
    setArchiveMeta(null)
    setArchiveTotal(0)
    setLines([])
    setEnded(false)
    get<{ archive: LogArchiveItem; lines: string[]; totalLines: number }>(
      `/log-archives/${encodeURIComponent(source)}?tail=${encodeURIComponent(tail)}`,
    )
      .then((res) => {
        if (cancelled) return
        setArchiveMeta(res.archive)
        setArchiveTotal(res.totalLines)
        setLines(res.lines || [])
      })
      .catch((err) => {
        if (cancelled) return
        setArchiveError(err)
        setArchiveMeta(null)
      })
      .finally(() => {
        if (!cancelled) setArchiveLoading(false)
      })
    return () => {
      cancelled = true
    }
  }, [source, tail])

  const isLive = source === 'live'
  const selectedArchive = isLive ? null : resolveActiveArchive(archiveMeta, archives || [], source)
  const shown = ts ? lines : lines.map(stripDockerTimestamp)

  return (
    <section className="card" style={{ overflow: 'hidden' }}>
      <div className="card-head">
        <div className="row wrap">
          {archives && archives.length > 0 && (
            <Select
              className="filter-select"
              style={{ width: 240 }}
              aria-label="日志来源"
              value={source}
              onChange={(v) => setSource(v)}
            >
              <option value="live">实时日志（当前容器）</option>
              {archives.map((a) => (
                <option key={a.id} value={a.id}>
                  {formatArchiveOption(a)}
                </option>
              ))}
            </Select>
          )}
          <input className="input filter-input" placeholder="筛选" aria-label="筛选日志" value={filter} onChange={(e) => setFilter(e.target.value)} />
          <Select className="filter-select" style={{ width: 130 }} aria-label="行数" value={tail} onChange={(v) => setTail(v)}>
            {['200', '500', '2000', '10000', 'all'].map((t) => <option key={t} value={t}>{t === 'all' ? '全部' : `最近 ${t} 行`}</option>)}
          </Select>
          {isLive && <label className="check"><input type="checkbox" checked={follow} onChange={(e) => setFollow(e.target.checked)} />实时跟随</label>}
          <label className="check"><input type="checkbox" checked={ts} onChange={(e) => setTs(e.target.checked)} />时间戳</label>
        </div>
        <div className="row wrap">
          {isLive && ended && <span className="label">流已结束</span>}
          {isLive && <button className="btn btn-sm" onClick={() => setPaused(!paused)}>{paused ? '继续' : '暂停'}</button>}
          {!isLive && canOperate && selectedArchive && (
            <button className="btn btn-sm btn-danger" onClick={() => setDeletingArchive(selectedArchive)}>
              <Icon name="trash" size={14} />删除归档
            </button>
          )}
          <a
            className="btn btn-sm"
            href={isLive ? `/api/containers/${id}/logs?download=1` : `/api/log-archives/${encodeURIComponent(source)}?download=1`}
          >
            <Icon name="download" size={16} />下载
          </a>
        </div>
      </div>
      {!isLive && selectedArchive && (
        <div
          className="row wrap"
          style={{
            padding: '8px 16px',
            borderBottom: '1px solid var(--line)',
            background: 'var(--bg)',
            fontSize: 13,
            gap: '6px 14px',
          }}
        >
          <span className="badge off">{archiveReasonLabel(selectedArchive.reason)}</span>
          <span className="muted">归档于 {dateTime(selectedArchive.archivedAt)}</span>
          <span className="mono muted">容器 {selectedArchive.containerId.slice(0, 12)}</span>
          {selectedArchive.image && <span className="mono muted">{selectedArchive.image}</span>}
          <span className="muted">
            共 {archiveTotal || selectedArchive.lineCount} 行 · {bytes(selectedArchive.sizeBytes)}
          </span>
          {selectedArchive.truncated && <span className="badge warn">已截断（仅保留最近 20,000 行）</span>}
        </div>
      )}
      {!isLive && archiveError ? (
        <div style={{ padding: 16 }}>
          <ErrorBox error={archiveError} />
        </div>
      ) : !isLive && archiveLoading ? (
        <Loading />
      ) : (
        <LogView lines={shown} height={560} follow={isLive && !paused} filter={filter} />
      )}
      {deletingArchive && (
        <Confirm
          title="删除历史日志归档"
          danger
          confirmText="删除"
          onClose={() => setDeletingArchive(null)}
          onConfirm={async () => {
            await del(`/log-archives/${encodeURIComponent(deletingArchive.id)}`)
            toast.ok('已删除历史日志归档')
            setSource('live')
            reloadArchives()
          }}
        >
          确定删除 <code className="mono">{deletingArchive.containerName}</code> 在 {dateTime(deletingArchive.archivedAt)} 的历史日志归档？删除后无法恢复。
        </Confirm>
      )}
    </section>
  )
}

function Term({ id }: { id: string }) {
  const ref = useRef<HTMLDivElement>(null)
  const termRef = useRef<Terminal | null>(null)
  const wsRef = useRef<WebSocket | null>(null)
  const sendKey = (seq: string) => {
    if (wsRef.current?.readyState === WebSocket.OPEN) {
      wsRef.current.send(JSON.stringify({ type: 'input', data: seq }))
    }
    termRef.current?.focus()
  }
  const [shell, setShell] = useState('auto')
  const [user, setUser] = useState('')
  const [session, setSession] = useState(0)
  const [state, setState] = useState<'connecting' | 'open' | 'closed'>('connecting')
  useEffect(() => {
    const el = ref.current
    if (!el) return
    let disposed = false
    let rafId = 0
    let term: Terminal | null = null
    let ws: WebSocket | null = null
    let ro: ResizeObserver | null = null
    let onDataSub: { dispose: () => void } | null = null
    setState('connecting')

    Promise.all([
      import('@xterm/xterm'),
      import('@xterm/addon-fit'),
      import('@xterm/xterm/css/xterm.css'),
    ])
      .then(([{ Terminal: XTerm }, { FitAddon }]) => {
        if (disposed || !el.isConnected) return
        term = new XTerm({
          fontFamily: "'IBM Plex Mono', 'Microsoft YaHei UI', 'PingFang SC', Consolas, monospace",
          fontSize: window.innerWidth <= 860 ? 12 : 13,
          cursorBlink: true,
          theme: { background: '#121315' },
          scrollback: 5000,
        })
        const fit = new FitAddon()
        term.loadAddon(fit)
        termRef.current = term
        const fitSafe = () => {
          if (disposed || !el.isConnected || el.clientWidth === 0 || el.clientHeight === 0) return false
          try {
            fit.fit()
            return true
          } catch {
            return false
          }
        }
        term.open(el)
        fitSafe()
        const proto = location.protocol === 'https:' ? 'wss:' : 'ws:'
        const qs = new URLSearchParams()
        if (shell !== 'auto') qs.set('shell', shell)
        if (user) qs.set('user', user)
        ws = new WebSocket(`${proto}//${location.host}/api/containers/${id}/terminal?${qs}`)
        ws.binaryType = 'arraybuffer'
        wsRef.current = ws
        const curTerm = term
        const curWs = ws
        const sendSize = () =>
          !disposed && curWs.readyState === WebSocket.OPEN && curWs.send(JSON.stringify({ type: 'resize', cols: curTerm.cols, rows: curTerm.rows }))
        rafId = requestAnimationFrame(() => {
          if (!disposed) {
            fitSafe()
            sendSize()
          }
        })
        curWs.onopen = () => {
          if (disposed) return
          setState('open')
          sendSize()
          curTerm.focus()
        }
        curWs.onmessage = (e) => {
          if (!disposed) curTerm.write(typeof e.data === 'string' ? e.data : new Uint8Array(e.data))
        }
        curWs.onclose = () => {
          if (disposed) return
          setState('closed')
          try {
            curTerm.write('\r\n\x1b[90m[连接已关闭]\x1b[0m\r\n')
          } catch {}
        }
        onDataSub = curTerm.onData((data) => !disposed && curWs.readyState === WebSocket.OPEN && curWs.send(JSON.stringify({ type: 'input', data })))
        ro = new ResizeObserver(() => {
          if (disposed) return
          fitSafe()
          sendSize()
        })
        ro.observe(el)
      })
      .catch(() => {
        if (!disposed) setState('closed')
      })

    return () => {
      disposed = true
      if (rafId) cancelAnimationFrame(rafId)
      if (ws) {
        ws.onopen = null
        ws.onmessage = null
        ws.onerror = null
        ws.onclose = null
        if (wsRef.current === ws) wsRef.current = null
        ws.close()
      }
      ro?.disconnect()
      onDataSub?.dispose()
      if (term) {
        if (termRef.current === term) termRef.current = null
        try {
          term.dispose()
        } catch {}
        // @xterm/xterm@5.5.0 Viewport schedules an untracked setTimeout(() => this.syncScrollArea())
        // in its constructor; silence it after dispose so React StrictMode remounts do not throw.
        const core = (term as unknown as { _core?: { viewport?: { syncScrollArea?: () => void } } })._core
        if (core?.viewport) core.viewport.syncScrollArea = () => {}
      }
    }
  }, [id, session]) // eslint-disable-line react-hooks/exhaustive-deps
  return (
    <section className="card" style={{ overflow: 'hidden' }}>
      <div className="card-head">
        <div className="row wrap">
          <Select className="filter-select" style={{ width: 140 }} aria-label="Shell" value={shell} onChange={(v) => setShell(v)}>
            <option value="auto">自动（bash/sh）</option>
            <option value="bash">bash</option>
            <option value="sh">sh</option>
          </Select>
          <input className="input filter-input" style={{ width: 'min(160px, 100%)' }} placeholder="用户（默认）" aria-label="用户" value={user} onChange={(e) => setUser(e.target.value)} />
          <button className="btn btn-sm" onClick={() => setSession(session + 1)}>{state === 'closed' ? '重新连接' : '重新打开'}</button>
        </div>
        <span className={'badge ' + (state === 'open' ? 'ok' : state === 'connecting' ? 'run' : 'off')}><span className="dot" />{state === 'open' ? '已连接' : state === 'connecting' ? '连接中' : '已断开'}</span>
      </div>
      <div className="term-keys" role="toolbar" aria-label="终端快捷键">
        <button type="button" onMouseDown={(e) => e.preventDefault()} onClick={() => sendKey('\x1b')}>Esc</button>
        <button type="button" onMouseDown={(e) => e.preventDefault()} onClick={() => sendKey('\t')}>Tab</button>
        <button type="button" onMouseDown={(e) => e.preventDefault()} onClick={() => sendKey('\x03')}>Ctrl+C</button>
        <button type="button" onMouseDown={(e) => e.preventDefault()} onClick={() => sendKey('\x04')}>Ctrl+D</button>
        <button type="button" onMouseDown={(e) => e.preventDefault()} onClick={() => sendKey('\x1b[A')}>↑</button>
        <button type="button" onMouseDown={(e) => e.preventDefault()} onClick={() => sendKey('\x1b[B')}>↓</button>
        <button type="button" onMouseDown={(e) => e.preventDefault()} onClick={() => sendKey('/')}>/</button>
        <button type="button" onMouseDown={(e) => e.preventDefault()} onClick={() => sendKey('|')}>|</button>
      </div>
      <div className="term"><div ref={ref} style={{ height: '100%' }} /></div>
      <div className="label" style={{ padding: '8px 16px' }}>终端会话的开始与结束会记录到审计日志。</div>
    </section>
  )
}

function Info({ c, id }: { c: any; id: string }) {
  const { data: st } = useLoad(() => get<any>(`/containers/${id}/stats`), [id], 5000)
  return (
    <>
      {st && (
        <div className="grid">
          <div className="card stat"><span className="label">CPU</span><div className="big">{st.cpuPercent.toFixed(1)}<small> %</small></div><Meter value={st.cpuPercent} max={100} /></div>
          <div className="card stat"><span className="label">内存</span><div className="big">{bytes(st.memUsage)}</div><Meter value={st.memUsage} max={st.memLimit} /><span className="label">上限 {bytes(st.memLimit)}</span></div>
          <div className="card stat"><span className="label">网络 收 / 发</span><div className="big" style={{ fontSize: 20 }}>{bytes(st.netRx)} / {bytes(st.netTx)}</div></div>
          <div className="card stat"><span className="label">磁盘 读 / 写</span><div className="big" style={{ fontSize: 20 }}>{bytes(st.blockRead)} / {bytes(st.blockWrite)}</div><span className="label">进程 {st.pids}</span></div>
        </div>
      )}
      <div className="grid-2">
        <Card title="基本信息">
          <dl className="kv">
            <dt>容器 ID</dt><dd className="mono">{c.id.slice(0, 12)}</dd>
            <dt>名称</dt><dd className="mono">{c.name}</dd>
            <dt>镜像</dt><dd className="mono">{c.image}</dd>
            <dt>状态</dt><dd>{c.state}{c.exitCode ? `（退出码 ${c.exitCode}）` : ''}</dd>
            <dt>启动于</dt><dd>{c.startedAt && !c.startedAt.startsWith('0001') ? dateTime(Date.parse(c.startedAt) / 1000) : '—'}</dd>
            <dt>重启策略</dt><dd className="mono">{c.restartPolicy || 'no'}</dd>
            <dt>内存限制</dt><dd>{c.memoryLimit ? bytes(c.memoryLimit) : '不限'}</dd>
            <dt>CPU 限制</dt><dd>{c.nanoCpus ? c.nanoCpus / 1e9 + ' 核' : '不限'}</dd>
            <dt>命令</dt><dd className="mono" style={{ overflowWrap: 'anywhere' }}>{[...(c.entrypoint || []), ...(c.cmd || [])].join(' ') || '—'}</dd>
          </dl>
        </Card>
        <Card title="网络与挂载">
          <div className="col" style={{ gap: 12 }}>
            {c.networks.map((n: any) => (
              <div key={n.name} style={{ overflowWrap: 'anywhere' }}><span className="tag">{n.name}</span> <span className="mono">{n.ip || '—'}</span> {n.aliases?.length > 0 && <span className="label">别名 {n.aliases.join(', ')}</span>}</div>
            ))}
            {c.mounts.length === 0 && <span className="muted">没有挂载</span>}
            {c.mounts.map((m: any, i: number) => (
              <div key={i} className="mono" style={{ fontSize: 13, overflowWrap: 'anywhere' }}>
                <span className="tag">{m.type}</span> {m.name || m.source} → {m.destination} {!m.rw && <span className="badge off">只读</span>}
              </div>
            ))}
          </div>
        </Card>
      </div>
      <Card title="环境变量" pad={false}>
        <ResponsiveTable
          table={
            <div className="table-wrap">
              <table>
                <tbody>
                  {(c.env || []).map((e: string, i: number) => {
                    const k = e.indexOf('=')
                    return <tr key={i}><td className="mono" style={{ width: '30%' }}>{k >= 0 ? e.slice(0, k) : e}</td><td className="mono" style={{ overflowWrap: 'anywhere' }}>{k >= 0 ? e.slice(k + 1) : ''}</td></tr>
                  })}
                </tbody>
              </table>
            </div>
          }
          cards={(c.env || []).map((e: string, i: number) => {
            const k = e.indexOf('=')
            return <MobileCard key={i} tight title={<span className="mono">{k >= 0 ? e.slice(0, k) : e}</span>} sub={<span className="mono">{k >= 0 ? e.slice(k + 1) : ''}</span>} />
          })}
        />
      </Card>
    </>
  )
}
