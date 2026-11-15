import { useState } from 'react'
import { bytes, get, qs } from '../api'
import { ErrorBox, Icon, Loading, Modal, useLoad } from '../components/ui'

type Entry = { ts: number; ip: string; method: string; host: string; uri: string; proto: string; status: number; size: number; durationMs: number; ua: string; referer: string }
type Resp = { enabled: boolean; entries: Entry[]; total: number; avgMs: number; classes: Record<string, number>; topPaths: { key: string; count: number }[]; topIps: { key: string; count: number }[]; files: number }

const statusKind = (s: number) => (s >= 500 ? 'err' : s >= 400 ? 'warn' : s >= 300 ? 'run' : 'ok')

function time(ts: number) {
  const d = new Date(ts * 1000)
  const p = (n: number) => String(n).padStart(2, '0')
  return `${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`
}

export function AccessLogs({ site, onClose }: { site: any; onClose: () => void }) {
  const [status, setStatus] = useState('')
  const [path, setPath] = useState('')
  const [ip, setIp] = useState('')
  const [hours, setHours] = useState(24)
  const [applied, setApplied] = useState({ status: '', path: '', ip: '' })
  const [sel, setSel] = useState<Entry | null>(null)
  const { data, error, loading, reload } = useLoad(
    () => get<Resp>(`/sites/${site.id}/access-logs` + qs({ ...applied, hours, limit: 300 })),
    [site.id, applied, hours],
  )
  const apply = () => setApplied({ status, path, ip })
  const total = data?.total || 0
  return (
    <Modal title={<>访问日志 · <span className="mono">{site.domains[0]}</span></>} wide onClose={onClose}>
      {data && !data.enabled && <div className="alert warn"><Icon name="alert" />这个站点没有开启访问日志，下面只显示之前记录的内容。在站点设置中开启「记录访问日志」。</div>}
      <form className="row wrap" onSubmit={(e) => { e.preventDefault(); apply() }}>
        <select className="select" style={{ width: 120 }} aria-label="时间范围" value={hours} onChange={(e) => setHours(Number(e.target.value))}>
          <option value={1}>1 小时</option>
          <option value={6}>6 小时</option>
          <option value={24}>24 小时</option>
          <option value={72}>3 天</option>
          <option value={168}>7 天</option>
        </select>
        <select className="select" style={{ width: 110 }} aria-label="状态码" value={status} onChange={(e) => { setStatus(e.target.value); setApplied({ status: e.target.value, path, ip }) }}>
          <option value="">全部状态</option>
          <option value="2xx">2xx</option>
          <option value="3xx">3xx</option>
          <option value="4xx">4xx</option>
          <option value="5xx">5xx</option>
        </select>
        <input className="input mono" style={{ width: 200 }} placeholder="路径包含" aria-label="路径" value={path} onChange={(e) => setPath(e.target.value)} />
        <input className="input mono" style={{ width: 150 }} placeholder="IP 前缀" aria-label="IP" value={ip} onChange={(e) => setIp(e.target.value)} />
        <button className="btn btn-sm" type="submit"><Icon name="search" size={16} />筛选</button>
        <button className="btn btn-sm btn-ghost btn-icon" type="button" aria-label="刷新" disabled={loading} onClick={reload}><Icon name="refresh" size={16} /></button>
      </form>
      {error ? <ErrorBox error={error} /> : !data ? <Loading /> : (
        <>
          <div className="grid" style={{ gap: 10, gridTemplateColumns: 'repeat(auto-fit, minmax(120px, 1fr))' }}>
            <div className="card stat" style={{ padding: 12 }}><span className="label">请求数</span><div className="big" style={{ fontSize: 20 }}>{total}</div></div>
            <div className="card stat" style={{ padding: 12 }}><span className="label">平均耗时</span><div className="big" style={{ fontSize: 20 }}>{data.avgMs}<small> ms</small></div></div>
            {['2xx', '3xx', '4xx', '5xx'].map((c) => (
              <div key={c} className="card stat" style={{ padding: 12 }}>
                <span className="label">{c}</span>
                <div className="big" style={{ fontSize: 20, color: c === '5xx' && data.classes[c] ? '#A41E14' : undefined }}>{data.classes[c] || 0}<small> {total ? Math.round(((data.classes[c] || 0) / total) * 100) : 0}%</small></div>
              </div>
            ))}
          </div>
          {total > 0 && (
            <div className="grid-2" style={{ gap: 12 }}>
              <TopList title="热门路径" items={data.topPaths} onPick={(k) => { setPath(k); setApplied({ status, path: k, ip }) }} />
              <TopList title="访问最多的 IP" items={data.topIps} onPick={(k) => { setIp(k); setApplied({ status, path, ip: k }) }} />
            </div>
          )}
          <section className="card" style={{ overflow: 'hidden' }}>
            <div className="table-wrap" style={{ maxHeight: 420, overflowY: 'auto' }}>
              <table className="sql-grid">
                <thead><tr><th>时间</th><th>状态</th><th>方法</th><th>路径</th><th>IP</th><th>大小</th><th>耗时</th></tr></thead>
                <tbody>
                  {data.entries.map((e, i) => (
                    <tr key={i} className="clickable" onClick={() => setSel(e)}>
                      <td>{time(e.ts)}</td>
                      <td><span className={'badge ' + statusKind(e.status)}>{e.status}</span></td>
                      <td>{e.method}</td>
                      <td title={e.uri} style={{ maxWidth: 320 }}>{e.uri}</td>
                      <td>{e.ip}</td>
                      <td>{bytes(e.size)}</td>
                      <td>{e.durationMs} ms</td>
                    </tr>
                  ))}
                  {data.entries.length === 0 && <tr><td colSpan={7} className="muted" style={{ textAlign: 'center', padding: 24, fontFamily: 'var(--font)' }}>没有匹配的请求</td></tr>}
                </tbody>
              </table>
            </div>
            <div className="label" style={{ padding: '8px 16px', borderTop: '1px solid var(--line-soft)' }}>显示最近 {data.entries.length} 条（共 {total} 条匹配）· 日志按 20MB 轮转，保留 7 天</div>
          </section>
          {sel && (
            <section className="card">
              <div className="card-head"><span className="st">请求详情</span><button className="btn btn-sm btn-ghost btn-icon" aria-label="关闭" onClick={() => setSel(null)}><Icon name="x" size={16} /></button></div>
              <div className="card-body">
                <dl className="kv">
                  <dt>时间</dt><dd>{new Date(sel.ts * 1000).toLocaleString()}</dd>
                  <dt>请求</dt><dd className="mono">{sel.method} {sel.host}{sel.uri} {sel.proto}</dd>
                  <dt>状态</dt><dd>{sel.status} · {bytes(sel.size)} · {sel.durationMs} ms</dd>
                  <dt>客户端</dt><dd className="mono">{sel.ip}</dd>
                  <dt>User-Agent</dt><dd className="mono" style={{ fontSize: 12 }}>{sel.ua || '—'}</dd>
                  <dt>Referer</dt><dd className="mono" style={{ fontSize: 12 }}>{sel.referer || '—'}</dd>
                </dl>
              </div>
            </section>
          )}
        </>
      )}
    </Modal>
  )
}

function TopList({ title, items, onPick }: { title: string; items: { key: string; count: number }[]; onPick: (k: string) => void }) {
  const max = items[0]?.count || 1
  return (
    <section className="card">
      <div className="card-head" style={{ padding: '10px 14px' }}><span className="st" style={{ fontSize: 14 }}>{title}</span></div>
      <div className="col" style={{ padding: '8px 14px 12px', gap: 6 }}>
        {items.map((x) => (
          <button key={x.key} className="fs-name" style={{ display: 'block', width: '100%' }} onClick={() => onPick(x.key)} title={x.key}>
            <div className="row" style={{ gap: 8 }}><span className="grow ellipsis">{x.key}</span><span className="label">{x.count}</span></div>
            <div className="track" style={{ height: 3, marginTop: 2 }}><div style={{ width: (x.count / max) * 100 + '%' }} /></div>
          </button>
        ))}
      </div>
    </section>
  )
}
