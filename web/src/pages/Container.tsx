import { useEffect, useRef, useState } from 'react'
import { Link, useNavigate, useParams, useSearchParams } from 'react-router-dom'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import '@xterm/xterm/css/xterm.css'
import { bytes, dateTime, get, post, streamLines } from '../api'
import { useCan } from '../auth'
import { Card, Confirm, ErrorBox, Icon, Loading, LogView, Meter, Page, Status, Tabs, useAction, useLoad } from '../components/ui'

type Tab = 'logs' | 'terminal' | 'info'

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
      sub={<span className="row wrap"><Status s={state} /><span className="mono">{c.image}</span>{c.restartCount > 0 && <span className="badge warn">重启 {c.restartCount} 次</span>}{c.oomKilled && <span className="badge err">OOM</span>}</span>}
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
        tabs={[['logs', '日志'], ...(can.operator ? [['terminal', '终端'] as [Tab, string]] : []), ['info', '详情与资源']]}
        value={tab}
        onChange={(t) => setParams({ tab: t }, { replace: true })}
      />
      {tab === 'logs' && <Logs id={id!} running={c.running} />}
      {tab === 'terminal' && (c.running ? <Term id={id!} /> : <div className="alert warn">容器未运行，无法打开终端。</div>)}
      {tab === 'info' && <Info c={c} id={id!} />}
      {removing && (
        <Confirm title={`删除容器 ${c.name}`} danger confirmText="删除" onClose={() => setRemoving(false)} onConfirm={async () => { await post(`/containers/${id}/remove`); nav(-1) }}>
          强制删除这个容器。如果它属于 Compose 项目，下次部署会重新创建。
        </Confirm>
      )}
    </Page>
  )
}

function Logs({ id, running }: { id: string; running: boolean }) {
  const [lines, setLines] = useState<string[]>([])
  const [tail, setTail] = useState('500')
  const [follow, setFollow] = useState(true)
  const [paused, setPaused] = useState(false)
  const [filter, setFilter] = useState('')
  const [ts, setTs] = useState(false)
  const [ended, setEnded] = useState(false)
  const pausedRef = useRef(false)
  pausedRef.current = paused
  useEffect(() => {
    const buf: string[] = []
    let raf = 0
    setLines([])
    setEnded(false)
    const close = streamLines(`/containers/${id}/logs?tail=${tail}&follow=${follow && running ? 1 : 0}`, (l) => {
      buf.push(l)
      if (buf.length > 20000) buf.splice(0, buf.length - 20000)
      if (!raf && !pausedRef.current) raf = requestAnimationFrame(() => { raf = 0; setLines([...buf]) })
    }, () => { setEnded(true); setLines([...buf]) })
    return () => { close(); if (raf) cancelAnimationFrame(raf) }
  }, [id, tail, follow, running])
  const shown = ts ? lines : lines.map((l) => l.replace(/^\d{4}-\d\d-\d\dT[\d:.]+Z\s/, ''))
  return (
    <section className="card" style={{ overflow: 'hidden' }}>
      <div className="card-head">
        <div className="row wrap">
          <input className="input" style={{ width: 220 }} placeholder="筛选" aria-label="筛选日志" value={filter} onChange={(e) => setFilter(e.target.value)} />
          <select className="select" style={{ width: 130 }} aria-label="行数" value={tail} onChange={(e) => setTail(e.target.value)}>
            {['200', '500', '2000', '10000', 'all'].map((t) => <option key={t} value={t}>{t === 'all' ? '全部' : `最近 ${t} 行`}</option>)}
          </select>
          <label className="check"><input type="checkbox" checked={follow} onChange={(e) => setFollow(e.target.checked)} />实时跟随</label>
          <label className="check"><input type="checkbox" checked={ts} onChange={(e) => setTs(e.target.checked)} />时间戳</label>
        </div>
        <div className="row">
          {ended && <span className="label">流已结束</span>}
          <button className="btn btn-sm" onClick={() => setPaused(!paused)}>{paused ? '继续' : '暂停'}</button>
          <a className="btn btn-sm" href={`/api/containers/${id}/logs?download=1`}><Icon name="download" size={16} />下载</a>
        </div>
      </div>
      <LogView lines={shown} height={560} follow={!paused} filter={filter} />
    </section>
  )
}

function Term({ id }: { id: string }) {
  const ref = useRef<HTMLDivElement>(null)
  const [shell, setShell] = useState('auto')
  const [user, setUser] = useState('')
  const [session, setSession] = useState(0)
  const [state, setState] = useState<'connecting' | 'open' | 'closed'>('connecting')
  useEffect(() => {
    if (!ref.current) return
    const term = new Terminal({ fontFamily: "'IBM Plex Mono', Consolas, monospace", fontSize: 13, cursorBlink: true, theme: { background: '#121315' }, scrollback: 5000 })
    const fit = new FitAddon()
    term.loadAddon(fit)
    term.open(ref.current)
    fit.fit()
    const proto = location.protocol === 'https:' ? 'wss:' : 'ws:'
    const qs = new URLSearchParams()
    if (shell !== 'auto') qs.set('shell', shell)
    if (user) qs.set('user', user)
    const ws = new WebSocket(`${proto}//${location.host}/api/containers/${id}/terminal?${qs}`)
    ws.binaryType = 'arraybuffer'
    setState('connecting')
    const sendSize = () => ws.readyState === WebSocket.OPEN && ws.send(JSON.stringify({ type: 'resize', cols: term.cols, rows: term.rows }))
    ws.onopen = () => {
      setState('open')
      sendSize()
      term.focus()
    }
    ws.onmessage = (e) => term.write(typeof e.data === 'string' ? e.data : new Uint8Array(e.data))
    ws.onclose = () => {
      setState('closed')
      term.write('\r\n\x1b[90m[连接已关闭]\x1b[0m\r\n')
    }
    const d = term.onData((data) => ws.readyState === WebSocket.OPEN && ws.send(JSON.stringify({ type: 'input', data })))
    const ro = new ResizeObserver(() => {
      try {
        fit.fit()
        sendSize()
      } catch {}
    })
    ro.observe(ref.current)
    return () => {
      ro.disconnect()
      d.dispose()
      ws.close()
      term.dispose()
    }
  }, [id, session]) // eslint-disable-line react-hooks/exhaustive-deps
  return (
    <section className="card" style={{ overflow: 'hidden' }}>
      <div className="card-head">
        <div className="row wrap">
          <select className="select" style={{ width: 140 }} aria-label="Shell" value={shell} onChange={(e) => setShell(e.target.value)}>
            <option value="auto">自动（bash/sh）</option>
            <option value="bash">bash</option>
            <option value="sh">sh</option>
          </select>
          <input className="input" style={{ width: 160 }} placeholder="用户（默认）" aria-label="用户" value={user} onChange={(e) => setUser(e.target.value)} />
          <button className="btn btn-sm" onClick={() => setSession(session + 1)}>{state === 'closed' ? '重新连接' : '重新打开'}</button>
        </div>
        <span className={'badge ' + (state === 'open' ? 'ok' : state === 'connecting' ? 'run' : 'off')}><span className="dot" />{state === 'open' ? '已连接' : state === 'connecting' ? '连接中' : '已断开'}</span>
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
            <dt>命令</dt><dd className="mono">{[...(c.entrypoint || []), ...(c.cmd || [])].join(' ') || '—'}</dd>
          </dl>
        </Card>
        <Card title="网络与挂载">
          <div className="col" style={{ gap: 12 }}>
            {c.networks.map((n: any) => (
              <div key={n.name}><span className="tag">{n.name}</span> <span className="mono">{n.ip || '—'}</span> {n.aliases?.length > 0 && <span className="label">别名 {n.aliases.join(', ')}</span>}</div>
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
        <div className="table-wrap">
          <table>
            <tbody>
              {(c.env || []).map((e: string, i: number) => {
                const k = e.indexOf('=')
                return <tr key={i}><td className="mono" style={{ width: '30%' }}>{e.slice(0, k)}</td><td className="mono" style={{ overflowWrap: 'anywhere' }}>{e.slice(k + 1)}</td></tr>
              })}
            </tbody>
          </table>
        </div>
      </Card>
    </>
  )
}
