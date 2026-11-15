import { ReactNode, useEffect, useMemo, useRef, useState } from 'react'
import { bytes, get } from '../api'
import { ErrorBox, Loading, useLoad } from './ui'

// ---------- line chart (hand-rolled SVG) ----------

export type Line = { label: string; values: (number | null)[]; color: string }

const PALETTE = ['#1D44B8', '#B54708', '#17663E', '#7A2E8E', '#0E7490', '#A41E14', '#5F6268', '#9A6B00']
export const color = (i: number) => PALETTE[i % PALETTE.length]

function niceMax(v: number) {
  if (v <= 0) return 1
  const p = Math.pow(10, Math.floor(Math.log10(v)))
  for (const m of [1, 1.2, 1.5, 2, 2.5, 3, 4, 5, 6, 8, 10]) if (m * p >= v) return m * p
  return 10 * p
}

function niceBytes(v: number) {
  if (v <= 0) return 1024
  const p = Math.pow(1024, Math.floor(Math.log(v) / Math.log(1024)))
  return niceMax(v / p) * p
}

function timeLabel(t: number, spanSec: number) {
  const d = new Date(t * 1000)
  const p = (n: number) => String(n).padStart(2, '0')
  if (spanSec > 2 * 86400) return `${d.getMonth() + 1}/${d.getDate()} ${p(d.getHours())}:00`
  return `${p(d.getHours())}:${p(d.getMinutes())}`
}

export function LineChart({ ts, lines, format, yMax, height = 160, unitBytes }: {
  ts: number[]
  lines: Line[]
  format: (v: number) => string
  yMax?: number
  height?: number
  unitBytes?: boolean
}) {
  const box = useRef<HTMLDivElement>(null)
  const [width, setWidth] = useState(600)
  const [hover, setHover] = useState<number | null>(null)
  useEffect(() => {
    if (!box.current) return
    const ro = new ResizeObserver((e) => setWidth(Math.max(200, Math.floor(e[0].contentRect.width))))
    ro.observe(box.current)
    return () => ro.disconnect()
  }, [])
  const padL = 58, padR = 10, padT = 10, padB = 22
  const w = width - padL - padR
  const h = height - padT - padB
  const maxV = useMemo(() => {
    if (yMax) return yMax
    let m = 0
    for (const l of lines) for (const v of l.values) if (v != null && v > m) m = v
    return unitBytes ? niceBytes(m) : niceMax(m)
  }, [lines, yMax, unitBytes])
  if (ts.length === 0) {
    return <div ref={box} className="chart-empty" style={{ height }}>暂无数据，每分钟采集一次</div>
  }
  const t0 = ts[0]
  const t1 = ts[ts.length - 1]
  const span = Math.max(t1 - t0, 60)
  // Expected interval between points; larger gaps break the line.
  const diffs = ts.slice(1).map((t, i) => t - ts[i]).sort((a, b) => a - b)
  const step = diffs.length ? diffs[Math.floor(diffs.length / 2)] : 60
  const x = (t: number) => padL + ((t - t0) / span) * w
  const y = (v: number) => padT + h - (Math.min(v, maxV) / maxV) * h
  const paths = lines.map((l) => {
    let d = ''
    let pen = false
    l.values.forEach((v, i) => {
      if (v == null || (i > 0 && ts[i] - ts[i - 1] > step * 2.5)) pen = false
      if (v == null) return
      d += (pen ? 'L' : 'M') + x(ts[i]).toFixed(1) + ',' + y(v).toFixed(1)
      pen = true
    })
    return d
  })
  const ticks = [0, 0.25, 0.5, 0.75, 1]
  const xt = Math.min(6, Math.max(2, Math.floor(w / 110)))
  const onMove = (e: React.MouseEvent<SVGSVGElement>) => {
    const r = (e.currentTarget as SVGSVGElement).getBoundingClientRect()
    const t = t0 + ((e.clientX - r.left - padL) / w) * span
    let best = 0
    for (let i = 1; i < ts.length; i++) if (Math.abs(ts[i] - t) < Math.abs(ts[best] - t)) best = i
    setHover(best)
  }
  return (
    <div ref={box} className="chart">
      <svg width={width} height={height} onMouseMove={onMove} onMouseLeave={() => setHover(null)} role="img">
        {ticks.map((k) => (
          <g key={k}>
            <line x1={padL} x2={padL + w} y1={padT + h - k * h} y2={padT + h - k * h} stroke="#EEEEEB" />
            <text x={padL - 6} y={padT + h - k * h + 4} textAnchor="end" className="chart-axis">{format(k * maxV)}</text>
          </g>
        ))}
        {Array.from({ length: xt + 1 }, (_, i) => t0 + (span * i) / xt).map((t, i) => (
          <text key={i} x={x(t)} y={height - 6} textAnchor={i === 0 ? 'start' : i === xt ? 'end' : 'middle'} className="chart-axis">{timeLabel(t, span)}</text>
        ))}
        {paths.map((d, i) => <path key={i} d={d} fill="none" stroke={lines[i].color} strokeWidth={1.6} strokeLinejoin="round" />)}
        {hover != null && (
          <g>
            <line x1={x(ts[hover])} x2={x(ts[hover])} y1={padT} y2={padT + h} stroke="#9C9EA3" strokeDasharray="3 3" />
            {lines.map((l, i) => l.values[hover] != null && <circle key={i} cx={x(ts[hover])} cy={y(l.values[hover]!)} r={3} fill={l.color} />)}
          </g>
        )}
      </svg>
      {hover != null && (
        <div className="chart-tip" style={{ left: Math.min(x(ts[hover]) + 12, width - 190) }}>
          <div className="label">{new Date(ts[hover] * 1000).toLocaleString()}</div>
          {lines.map((l, i) => (
            <div key={i} className="row" style={{ gap: 6 }}>
              <span className="chart-key" style={{ background: l.color }} />
              <span className="grow ellipsis">{l.label}</span>
              <b>{l.values[hover] == null ? '—' : format(l.values[hover]!)}</b>
            </div>
          ))}
        </div>
      )}
      {lines.length > 1 && (
        <div className="row wrap chart-legend">
          {lines.map((l, i) => <span key={i} className="row" style={{ gap: 6 }}><span className="chart-key" style={{ background: l.color }} />{l.label}</span>)}
        </div>
      )}
    </div>
  )
}

// ---------- metric panels ----------

export type MetricSeries = {
  name: string
  ts: number[]
  cpu: number[]
  mem: number[]
  memLimit: number[]
  netRx: number[]
  netTx: number[]
  blkR: number[]
  blkW: number[]
  disk?: number[]
  diskSize?: number[]
}

export type Range = '1h' | '6h' | '24h' | '7d'
const RANGES: [Range, string][] = [['1h', '1 小时'], ['6h', '6 小时'], ['24h', '24 小时'], ['7d', '7 天']]

export function RangePicker({ value, onChange }: { value: Range; onChange: (r: Range) => void }) {
  return (
    <div className="seg" role="group" aria-label="时间范围">
      {RANGES.map(([k, t]) => <button key={k} className={value === k ? 'on' : ''} onClick={() => onChange(k)}>{t}</button>)}
    </div>
  )
}

const pctFmt = (v: number) => (v >= 10 ? v.toFixed(0) : v.toFixed(1)) + '%'
const rateFmt = (v: number) => bytes(v) + '/s'

function ChartCard({ title, children, extra }: { title: string; children: ReactNode; extra?: ReactNode }) {
  return (
    <section className="card">
      <div className="card-head" style={{ padding: '10px 16px' }}><span className="st" style={{ fontSize: 14 }}>{title}</span>{extra}</div>
      <div style={{ padding: '8px 12px 10px' }}>{children}</div>
    </section>
  )
}

const last = (a?: number[]) => (a && a.length ? a[a.length - 1] : undefined)

// Charts for one host or container series.
export function MetricsPanel({ url, host }: { url: string; host?: boolean }) {
  const [range, setRange] = useState<Range>('1h')
  const { data, error } = useLoad(() => get<MetricSeries>(`${url}?range=${range}`), [url, range], 60000)
  return (
    <div className="col" style={{ gap: 12 }}>
      <div className="row wrap">
        <span className="st">资源曲线</span>
        <span className="label">每分钟采集，保留 7 天</span>
        <span className="spacer" />
        <RangePicker value={range} onChange={setRange} />
      </div>
      {error ? <ErrorBox error={error} /> : !data ? <Loading /> : <SeriesCharts s={data} host={host} />}
    </div>
  )
}

function SeriesCharts({ s, host }: { s: MetricSeries; host?: boolean }) {
  const memLimit = last(s.memLimit) || 0
  // Docker reports the host's memory as the limit when none is set.
  const memMax = memLimit > 0 && memLimit < 1 << 50 ? memLimit : undefined
  return (
    <div className="grid-2" style={{ gap: 12 }}>
      <ChartCard title="CPU" extra={<span className="label">当前 {last(s.cpu) != null ? pctFmt(last(s.cpu)!) : '—'}</span>}>
        <LineChart ts={s.ts} lines={[{ label: 'CPU', values: s.cpu, color: color(0) }]} format={pctFmt} yMax={host ? 100 : undefined} />
      </ChartCard>
      <ChartCard title="内存" extra={<span className="label">当前 {bytes(last(s.mem))}{memMax ? ` / ${bytes(memMax)}` : ''}</span>}>
        <LineChart ts={s.ts} lines={[{ label: '已用', values: s.mem, color: color(1) }]} format={(v) => bytes(v)} yMax={host ? memMax : undefined} unitBytes />
      </ChartCard>
      {host ? (
        <ChartCard title="磁盘（数据目录）" extra={<span className="label">已用 {bytes(last(s.disk))} / {bytes(last(s.diskSize))}</span>}>
          <LineChart ts={s.ts} lines={[{ label: '已用', values: s.disk || [], color: color(2) }]} format={(v) => bytes(v)} yMax={last(s.diskSize) || undefined} unitBytes />
        </ChartCard>
      ) : (
        <>
          <ChartCard title="网络">
            <LineChart ts={s.ts} lines={[{ label: '接收', values: s.netRx, color: color(0) }, { label: '发送', values: s.netTx, color: color(1) }]} format={rateFmt} unitBytes />
          </ChartCard>
          <ChartCard title="磁盘 IO">
            <LineChart ts={s.ts} lines={[{ label: '读', values: s.blkR, color: color(2) }, { label: '写', values: s.blkW, color: color(3) }]} format={rateFmt} unitBytes />
          </ChartCard>
        </>
      )}
    </div>
  )
}

// Aligns several series on a common time axis.
function align(list: MetricSeries[], pick: (s: MetricSeries) => number[]) {
  const all = new Set<number>()
  list.forEach((s) => s.ts.forEach((t) => all.add(t)))
  const ts = [...all].sort((a, b) => a - b)
  const idx = new Map(ts.map((t, i) => [t, i]))
  const lines = list.map((s, i) => {
    const values: (number | null)[] = ts.map(() => null)
    const v = pick(s)
    s.ts.forEach((t, j) => (values[idx.get(t)!] = v[j]))
    return { label: s.name, values, color: color(i) }
  })
  return { ts, lines }
}

// Per-container curves of a project, one line per container.
export function ProjectMetrics({ projectId }: { projectId: number | string }) {
  const [range, setRange] = useState<Range>('1h')
  const { data, error } = useLoad(() => get<MetricSeries[]>(`/projects/${projectId}/metrics?range=${range}`), [projectId, range], 60000)
  return (
    <div className="col" style={{ gap: 12 }}>
      <div className="row wrap">
        <span className="st">资源曲线</span>
        <span className="label">按容器，每分钟采集，保留 7 天</span>
        <span className="spacer" />
        <RangePicker value={range} onChange={setRange} />
      </div>
      {error ? <ErrorBox error={error} /> : !data ? <Loading /> : (
        <div className="grid-2" style={{ gap: 12 }}>
          <ChartCard title="CPU"><LineChart {...align(data, (s) => s.cpu)} format={pctFmt} /></ChartCard>
          <ChartCard title="内存"><LineChart {...align(data, (s) => s.mem)} format={(v) => bytes(v)} unitBytes /></ChartCard>
          <ChartCard title="网络接收"><LineChart {...align(data, (s) => s.netRx)} format={rateFmt} unitBytes /></ChartCard>
          <ChartCard title="网络发送"><LineChart {...align(data, (s) => s.netTx)} format={rateFmt} unitBytes /></ChartCard>
        </div>
      )}
    </div>
  )
}
