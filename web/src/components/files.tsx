import { useRef, useState } from 'react'
import { bytes, dateTime, get, qs, upload } from '../api'
import { ErrorBox, Icon, Loading, useLoad, useToast } from './ui'
import { MobileCard, ResponsiveTable, joinInline } from './ResponsiveList'

type FSEntry = { name: string; type: 'dir' | 'file' | 'link' | 'other'; dir: boolean; size: number; mtime: number; mode: string }
type Listing = { path: string; entries: FSEntry[]; truncated: boolean; via: string }

const join = (dir: string, name: string) => (dir.endsWith('/') ? dir : dir + '/') + name

// Browses a container filesystem or a volume. `base` is the API prefix,
// e.g. /containers/<id>/fs or /docker/volumes/<name>/fs.
export function FileBrowser({ base, canWrite, initial = '/', note }: { base: string; canWrite?: boolean; initial?: string; note?: string }) {
  const [path, setPath] = useState(initial)
  const [input, setInput] = useState(initial)
  const [filter, setFilter] = useState('')
  const { data, error, loading, reload } = useLoad(() => get<Listing>(`${base}${qs({ path })}`), [base, path])
  const fileRef = useRef<HTMLInputElement>(null)
  const [progress, setProgress] = useState<number | null>(null)
  const toast = useToast()
  const go = (p: string) => {
    setPath(p)
    setInput(p)
    setFilter('')
  }
  const parts = path.split('/').filter(Boolean)
  const doUpload = async (files: FileList | null) => {
    if (!files || files.length === 0) return
    try {
      for (const f of Array.from(files)) {
        setProgress(0)
        await upload('PUT', `${base}/upload${qs({ path, name: f.name })}`, f, setProgress)
      }
      toast.ok(files.length > 1 ? `已上传 ${files.length} 个文件` : `已上传 ${files[0].name}`)
      reload()
    } catch (e) {
      toast.err(e)
    } finally {
      setProgress(null)
      if (fileRef.current) fileRef.current.value = ''
    }
  }
  const entries = (data?.entries || []).filter((e) => !filter || e.name.toLowerCase().includes(filter.toLowerCase()))
  const dl = (p: string) => `/api${base}/download${qs({ path: p })}`
  return (
    <section className="card" style={{ overflow: 'hidden' }}>
      <div className="card-head">
        <form className="row grow" style={{ minWidth: 'min(220px, 100%)', flex: '1 1 200px' }} onSubmit={(e) => { e.preventDefault(); go(input.trim() || '/') }}>
          <input className="input mono" aria-label="路径" value={input} onChange={(e) => setInput(e.target.value)} />
          <button className="btn btn-sm" type="submit">前往</button>
        </form>
        <div className="row wrap">
          <input className="input filter-input" style={{ flex: '1 1 130px' }} placeholder="筛选" aria-label="筛选文件" value={filter} onChange={(e) => setFilter(e.target.value)} />
          <button className="btn btn-sm btn-icon" title="刷新" aria-label="刷新" onClick={reload}><Icon name="refresh" size={16} /></button>
          <a className="btn btn-sm" href={dl(path)} title="下载整个目录（tar）"><Icon name="download" size={16} />打包下载</a>
          {canWrite && (
            <>
              <button className="btn btn-sm" disabled={progress != null} onClick={() => fileRef.current?.click()}><Icon name="upload" size={16} />上传</button>
              <input ref={fileRef} type="file" multiple hidden onChange={(e) => doUpload(e.target.files)} />
            </>
          )}
        </div>
      </div>
      {progress != null && <div className="progress"><div style={{ width: progress + '%' }} /></div>}
      <div className="row wrap" style={{ padding: '8px 16px', borderBottom: '1px solid var(--line-soft)' }}>
        <div className="fs-path">
          <button onClick={() => go('/')}>/</button>
          {parts.map((p, i) => (
            <span key={i} className="row" style={{ gap: 2 }}>
              <button onClick={() => go('/' + parts.slice(0, i + 1).join('/'))}>{p}</button>
              {i < parts.length - 1 && <span className="muted">/</span>}
            </span>
          ))}
        </div>
        <span className="spacer" />
        {data && <span className="label">{data.entries.length} 项{data.via === 'archive' ? '（容器未运行，通过归档读取）' : ''}</span>}
      </div>
      {error ? <div className="card-body"><ErrorBox error={error} /></div> : !data || loading ? <Loading /> : (
        <ResponsiveTable
          cardsStyle={{ maxHeight: '62dvh', overflowY: 'auto' }}
          table={
            <div className="table-wrap" style={{ maxHeight: 560, overflowY: 'auto' }}>
              <table>
                <thead><tr><th>名称</th><th>大小</th><th>权限</th><th>修改时间</th><th /></tr></thead>
                <tbody>
                  {path !== '/' && (
                    <tr><td colSpan={5}><button className="fs-name" onClick={() => go('/' + parts.slice(0, -1).join('/'))}><Icon name="folder" size={16} />..</button></td></tr>
                  )}
                  {entries.map((e) => (
                    <tr key={e.name}>
                      <td>
                        {e.dir ? (
                          <button className="fs-name" onClick={() => go(join(path, e.name))}><Icon name="folder" size={16} />{e.name}{e.type === 'link' && <span className="label">→</span>}</button>
                        ) : (
                          <span className="fs-name" style={{ cursor: 'default' }}><Icon name={e.type === 'link' ? 'link' : 'file'} size={16} />{e.name}</span>
                        )}
                      </td>
                      <td className="nowrap">{e.dir ? '—' : bytes(e.size)}</td>
                      <td className="mono nowrap">{e.mode}</td>
                      <td className="nowrap">{e.mtime ? dateTime(e.mtime) : '—'}</td>
                      <td style={{ textAlign: 'right' }}>
                        {e.type !== 'other' && <a className="btn btn-sm btn-ghost btn-icon" href={dl(join(path, e.name))} title={e.dir ? '打包下载' : '下载'} aria-label="下载"><Icon name="download" size={16} /></a>}
                      </td>
                    </tr>
                  ))}
                  {entries.length === 0 && <tr><td colSpan={5} className="muted" style={{ textAlign: 'center', padding: 24 }}>空目录</td></tr>}
                </tbody>
              </table>
            </div>
          }
          cards={
            <>
              {path !== '/' && (
                <div className="m-card m-card-tight clickable" onClick={() => go('/' + parts.slice(0, -1).join('/'))}>
                  <span className="fs-name"><Icon name="folder" size={16} />..（上级目录）</span>
                </div>
              )}
              {entries.map((e) => (
                <MobileCard
                  key={e.name}
                  tight
                  onClick={e.dir ? () => go(join(path, e.name)) : undefined}
                  title={
                    <span className="fs-name" style={e.dir ? undefined : { cursor: 'default' }}>
                      <Icon name={e.dir ? 'folder' : e.type === 'link' ? 'link' : 'file'} size={16} />
                      {e.name}{e.type === 'link' && <span className="label">→</span>}
                    </span>
                  }
                  sub={joinInline([
                    !e.dir && bytes(e.size),
                    <span key="m" className="mono">{e.mode}</span>,
                    e.mtime ? dateTime(e.mtime) : false,
                  ])}
                  badges={
                    e.type !== 'other' ? (
                      <a
                        className="btn btn-sm btn-ghost btn-icon"
                        href={dl(join(path, e.name))}
                        title={e.dir ? '打包下载' : '下载'}
                        aria-label="下载"
                        onClick={(ev) => ev.stopPropagation()}
                      >
                        <Icon name="download" size={16} />
                      </a>
                    ) : undefined
                  }
                />
              ))}
              {entries.length === 0 && <div className="muted" style={{ textAlign: 'center', padding: 24 }}>空目录</div>}
            </>
          }
        />
      )}
      {(data?.truncated || note) && (
        <div className="label" style={{ padding: '8px 16px', borderTop: '1px solid var(--line-soft)' }}>
          {data?.truncated && '条目过多，只显示前 3000 项。'}{note}
        </div>
      )}
    </section>
  )
}
