import { createContext, CSSProperties, ReactNode, useCallback, useContext, useEffect, useRef, useState } from 'react'
import { get, post, streamLines } from '../../api'
import { lockBodyScroll, useEscapeLayer } from './hooks'
import { Icon } from './icons'
import { JobStatus } from './primitives'

const JobCtx = createContext<(id: string, title?: string) => void>(() => {})
export const useJobLog = () => useContext(JobCtx)

export function JobLogProvider({ children }: { children: ReactNode }) {
  const [job, setJob] = useState<{ id: string; title?: string } | null>(null)
  const open = useCallback((id: string, title?: string) => setJob({ id, title }), [])
  return (
    <JobCtx.Provider value={open}>
      {children}
      {job && <JobDrawer id={job.id} title={job.title} onClose={() => setJob(null)} />}
    </JobCtx.Provider>
  )
}

export function lineClass(l: string) {
  if (/^(==>|▶|\[step\])/.test(l)) return 'ln-step'
  if (/^\$ /.test(l)) return 'ln-cmd'
  if (/(error|错误|失败|failed|fatal)/i.test(l)) return 'ln-err'
  if (/^(✓|完成|成功)/.test(l)) return 'ln-ok'
  return ''
}

export function LogView({ lines, height = 420, follow = true, filter }: { lines: string[]; height?: number | string; follow?: boolean; filter?: string }) {
  const ref = useRef<HTMLDivElement>(null)
  useEffect(() => {
    if (follow && ref.current) ref.current.scrollTop = ref.current.scrollHeight
  }, [lines, follow])
  const shown = filter ? lines.filter((l) => l.toLowerCase().includes(filter.toLowerCase())) : lines
  return (
    <div ref={ref} className="log" style={{ '--log-h': typeof height === 'number' ? `${height}px` : height } as CSSProperties}>
      {shown.length === 0 ? <span style={{ color: '#888' }}>（暂无输出）</span> : shown.map((l, i) => <div key={i} className={lineClass(l)}>{l || ' '}</div>)}
    </div>
  )
}

function JobDrawer({ id, title, onClose }: { id: string; title?: string; onClose: () => void }) {
  const [lines, setLines] = useState<string[]>([])
  const [status, setStatus] = useState('running')
  const [job, setJob] = useState<any>()
  useEffect(() => lockBodyScroll(), [])
  useEscapeLayer(true, onClose)
  useEffect(() => {
    setLines([])
    const buf: string[] = []
    let raf = 0
    const flush = () => {
      raf = 0
      setLines([...buf])
    }
    const reset = () => {
      buf.length = 0
      if (raf) {
        cancelAnimationFrame(raf)
        raf = 0
      }
      setLines([])
    }
    const close = streamLines(
      `/jobs/${id}/log`,
      (l) => {
        buf.push(l)
        if (!raf) raf = requestAnimationFrame(flush)
      },
      (st) => {
        setStatus(st || 'done')
        get(`/jobs/${id}`).then(setJob).catch(() => {})
      },
      reset,
    )
    get(`/jobs/${id}`).then(setJob).catch(() => {})
    return () => {
      close()
      if (raf) cancelAnimationFrame(raf)
    }
  }, [id])
  const cancel = async () => {
    await post(`/jobs/${id}/cancel`).catch(() => {})
  }
  const st = job?.status && job.status !== 'running' ? job.status : status === 'running' ? 'running' : job?.status || status
  return (
    <>
      <div className="overlay" style={{ background: 'rgba(24,25,27,.25)' }} onMouseDown={onClose} />
      <aside className="drawer" role="dialog" aria-label="任务日志">
        <div className="modal-head">
          <div className="grow" style={{ minWidth: 0 }}>
            <div className="st ellipsis">{title || job?.title || '任务日志'}</div>
            <div className="label mono ellipsis">{id}</div>
          </div>
          <JobStatus s={st} />
          {st === 'running' && <button className="btn btn-sm" onClick={cancel}>取消任务</button>}
          <button className="btn btn-ghost btn-sm btn-icon" aria-label="关闭" onClick={onClose}><Icon name="x" /></button>
        </div>
        {job?.error && <div className="alert err" style={{ margin: 12 }}>{job.error}</div>}
        <LogView lines={lines} height="100%" />
      </aside>
    </>
  )
}
