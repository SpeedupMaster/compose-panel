import { useEffect, useState } from 'react'
import { downloadCSV, get, post, qs, upload } from '../api'
import { useCan } from '../auth'
import { CodeArea } from '../components/editors'
import { Card, Confirm, Empty, ErrorBox, Field, FilePicker, Icon, Loading, Select, Tabs, useJobLog, useLoad, useToast } from '../components/ui'

type QueryResult = { columns?: string[]; rows?: string[][]; truncated?: boolean; elapsedMs?: number; error?: string; needConfirm?: boolean; reasons?: string[] }

function DbPicker({ id, value, onChange }: { id: string; value: string; onChange: (v: string) => void }) {
  const { data: dbs } = useLoad(() => get<any[]>(`/databases/${id}/dbs`), [id])
  const cur = value || dbs?.[0]?.name || ''
  useEffect(() => {
    if (!value && cur) onChange(cur)
  }, [value, cur]) // eslint-disable-line react-hooks/exhaustive-deps
  return (
    <Select aria-label="库" value={cur} onChange={(v) => onChange(v)}>
      {(dbs || []).map((x) => <option key={x.name}>{x.name}</option>)}
    </Select>
  )
}

function ResultGrid({ res }: { res: QueryResult }) {
  if (res.error) return <pre className="alert err mono" style={{ margin: 12, whiteSpace: 'pre-wrap' }}>{res.error}</pre>
  const cols = res.columns || []
  const rows = res.rows || []
  if (cols.length === 0) return <div className="card-body muted">语句已执行，没有返回结果集。</div>
  return (
    <div className="table-wrap" style={{ maxHeight: 480, overflowY: 'auto' }}>
      <table className="sql-grid">
        <thead><tr>{cols.map((c, i) => <th key={i}>{c}</th>)}</tr></thead>
        <tbody>
          {rows.map((r, i) => <tr key={i}>{r.map((v, j) => <td key={j} title={v}>{v}</td>)}</tr>)}
          {rows.length === 0 && <tr><td colSpan={cols.length} className="muted">0 行</td></tr>}
        </tbody>
      </table>
    </div>
  )
}

// ---------- SQL console ----------

export function SqlConsole({ id, instance }: { id: string; instance: string }) {
  const can = useCan()
  const [db, setDb] = useState('')
  const { data: tables } = useLoad(async () => (db ? await get<any[]>(`/databases/${id}/tables${qs({ db })}`) : []), [id, db])
  const [sql, setSql] = useState('SELECT 1;')
  const [ro, setRo] = useState(true)
  const [res, setRes] = useState<QueryResult | null>(null)
  const [busy, setBusy] = useState(false)
  const [confirm, setConfirm] = useState<string[] | null>(null)
  const readOnly = ro || !can.operator
  const runQuery = async (confirmed = false) => {
    setBusy(true)
    try {
      const r = await post<QueryResult>(`/databases/${id}/query`, { database: db, sql, readOnly, confirm: confirmed })
      if (r.needConfirm) setConfirm(r.reasons || [])
      else setRes(r)
    } catch (e) {
      setRes({ error: (e as Error).message })
    } finally {
      setBusy(false)
    }
  }
  return (
    <div className="row" style={{ alignItems: 'flex-start', gap: 16, flexWrap: 'wrap' }}>
      <Card title="表">
        <div className="col" style={{ width: 220, gap: 8 }}>
          <DbPicker id={id} value={db} onChange={setDb} />
          <div className="col" style={{ gap: 0, maxHeight: 460, overflow: 'auto' }}>
            {(tables || []).map((t) => (
              <button key={t.name} className="btn btn-ghost btn-sm" style={{ justifyContent: 'space-between' }} onClick={() => setSql(`SELECT * FROM ${t.name} LIMIT 100;`)}>
                <span className="mono ellipsis">{t.name}</span><span className="label">{t.rows >= 0 ? t.rows : ''}</span>
              </button>
            ))}
            {tables?.length === 0 && <span className="label">没有表</span>}
          </div>
        </div>
      </Card>
      <div className="col grow" style={{ minWidth: 320, gap: 12 }}>
        <section className="card">
          <div className="card-body col" style={{ gap: 10 }}>
            <CodeArea label="SQL" lang="sql" value={sql} onChange={setSql} onSubmit={() => runQuery()} rows={8} />
            <div className="row wrap">
              <label className="check"><input type="checkbox" checked={readOnly} disabled={!can.operator} onChange={(e) => setRo(e.target.checked)} />只读事务</label>
              <span className="label">Ctrl+Enter 执行 · 超时 30 秒 · 最多返回 1000 行 · 所有语句记录审计</span>
              <span className="spacer" />
              <button className="btn btn-primary" disabled={busy || !db} onClick={() => runQuery()}><Icon name="play" size={16} />{busy ? '执行中…' : '执行'}</button>
            </div>
            {!readOnly && <div className="alert warn" style={{ padding: '8px 12px' }}><Icon name="alert" />读写模式：DROP / TRUNCATE / 不带 WHERE 的 UPDATE、DELETE 等危险语句需要二次确认。</div>}
          </div>
        </section>
        {res && (
          <section className="card" style={{ overflow: 'hidden' }}>
            <div className="card-head">
              <span className="label">{res.error ? '执行出错' : `${res.rows?.length ?? 0} 行${res.truncated ? '（已截断到 1000 行）' : ''}`} · {res.elapsedMs ?? '—'} ms</span>
              {!res.error && (res.columns?.length || 0) > 0 && (
                <button className="btn btn-sm" onClick={() => downloadCSV(`${instance}-${db}-${Date.now()}.csv`, res.columns!, res.rows || [])}><Icon name="download" size={16} />导出 CSV</button>
              )}
            </div>
            <ResultGrid res={res} />
          </section>
        )}
      </div>
      {confirm && (
        <Confirm title="确认执行危险语句" danger confirmText="仍然执行" onClose={() => setConfirm(null)} onConfirm={() => runQuery(true)}>
          <div className="alert err"><Icon name="alert" /><div>这条 SQL 包含可能破坏数据的操作：<ul style={{ margin: '6px 0 0', paddingLeft: 18 }}>{confirm.map((r, i) => <li key={i}>{r}</li>)}</ul></div></div>
          <p className="label" style={{ margin: 0 }}>库：<b className="mono">{db}</b>。执行后无法撤销，建议先备份。</p>
        </Confirm>
      )}
    </div>
  )
}

// ---------- table browser ----------

export function TableBrowser({ id }: { id: string }) {
  const [db, setDb] = useState('')
  const [table, setTable] = useState('')
  const [filter, setFilter] = useState('')
  const { data: tables, error } = useLoad(async () => (db ? await get<any[]>(`/databases/${id}/tables${qs({ db })}`) : null), [id, db])
  const list = (tables || []).filter((t) => !filter || t.name.toLowerCase().includes(filter.toLowerCase()))
  return (
    <div className="row" style={{ alignItems: 'flex-start', gap: 16, flexWrap: 'wrap' }}>
      <Card title="表">
        <div className="col" style={{ width: 240, gap: 8 }}>
          <DbPicker id={id} value={db} onChange={(v) => { setDb(v); setTable('') }} />
          <input className="input" placeholder="筛选表" aria-label="筛选表" value={filter} onChange={(e) => setFilter(e.target.value)} />
          <div className="col" style={{ gap: 0, maxHeight: 520, overflow: 'auto' }}>
            {error ? <ErrorBox error={error} /> : !tables ? <Loading /> : list.map((t) => (
              <button key={t.name} className={'btn btn-sm ' + (t.name === table ? 'btn-primary' : 'btn-ghost')} style={{ justifyContent: 'space-between' }} onClick={() => setTable(t.name)}>
                <span className="mono ellipsis">{t.name}</span><span className="label" style={{ color: 'inherit', opacity: 0.7 }}>{t.rows >= 0 ? t.rows : ''}</span>
              </button>
            ))}
            {tables?.length === 0 && <span className="label">没有表</span>}
          </div>
        </div>
      </Card>
      <div className="col grow" style={{ minWidth: 320 }}>
        {table ? <TableView key={db + '/' + table} id={id} db={db} table={table} /> : <section className="card"><Empty>选择左侧的表查看结构和数据。</Empty></section>}
      </div>
    </div>
  )
}

function TableView({ id, db, table }: { id: string; db: string; table: string }) {
  const [tab, setTab] = useState<'data' | 'columns' | 'indexes'>('data')
  const [page, setPage] = useState(1)
  const size = 50
  const base = `/databases/${id}/tables/${encodeURIComponent(table)}`
  const { data: info, error: infoErr } = useLoad(() => get<any>(base + qs({ db })), [base, db])
  const { data: rows, error: rowsErr, loading } = useLoad(() => get<any>(`${base}/rows` + qs({ db, page, size })), [base, db, page])
  return (
    <section className="card" style={{ overflow: 'hidden' }}>
      <div className="card-head" style={{ paddingBottom: 0, borderBottom: 0 }}>
        <span className="st mono">{table}</span>
        {tab === 'data' && rows && (
          <div className="row">
            <button className="btn btn-sm" disabled={page <= 1 || loading} onClick={() => setPage(page - 1)}>上一页</button>
            <span className="label">第 {page} 页 · 每页 {size} 行</span>
            <button className="btn btn-sm" disabled={(rows.rows?.length || 0) < size || loading} onClick={() => setPage(page + 1)}>下一页</button>
            <button className="btn btn-sm" disabled={!rows.columns?.length} onClick={() => downloadCSV(`${db}-${table}-p${page}.csv`, rows.columns, rows.rows || [])}><Icon name="download" size={16} />CSV</button>
          </div>
        )}
      </div>
      <div style={{ padding: '0 16px' }}>
        <Tabs tabs={[['data', '数据'], ['columns', `字段${info ? `（${info.columns.length}）` : ''}`], ['indexes', `索引${info ? `（${info.indexes.length}）` : ''}`]]} value={tab} onChange={setTab} />
      </div>
      {tab === 'data' && (rowsErr ? <div className="card-body"><ErrorBox error={rowsErr} /></div> : !rows ? <Loading /> : <ResultGrid res={rows} />)}
      {tab !== 'data' && (infoErr ? <div className="card-body"><ErrorBox error={infoErr} /></div> : !info ? <Loading /> : tab === 'columns' ? (
        <div className="table-wrap">
          <table className="sql-grid">
            <thead><tr><th>字段</th><th>类型</th><th>可空</th><th>默认值</th><th>键</th><th>其他</th></tr></thead>
            <tbody>
              {info.columns.map((c: any) => (
                <tr key={c.name}>
                  <td style={{ fontWeight: 600 }}>{c.name}</td>
                  <td>{c.type}</td>
                  <td>{c.nullable ? '是' : '否'}</td>
                  <td title={c.default}>{c.default || <span className="muted">—</span>}</td>
                  <td>{c.key && <span className="tag">{c.key}</span>}</td>
                  <td>{c.extra}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : (
        <div className="table-wrap">
          {info.indexes.length === 0 ? <Empty>没有索引。</Empty> : (
            <table className="sql-grid">
              <thead><tr><th>索引</th><th>字段</th><th>唯一</th><th>定义</th></tr></thead>
              <tbody>
                {info.indexes.map((x: any) => (
                  <tr key={x.name}>
                    <td style={{ fontWeight: 600 }}>{x.name}</td>
                    <td>{x.columns}</td>
                    <td>{x.unique ? <span className="badge ok">唯一</span> : '—'}</td>
                    <td title={x.def}>{x.def || '—'}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>
      ))}
    </section>
  )
}

// ---------- slow queries ----------

export function SlowQueries({ id, engine }: { id: string; engine: string }) {
  const { data, error, reload, loading } = useLoad(() => get<{ items: any[]; hint: string }>(`/databases/${id}/slow-queries`), [id])
  return (
    <Card title="慢查询（按总耗时排序）" pad={false} actions={<button className="btn btn-sm" disabled={loading} onClick={reload}><Icon name="refresh" size={16} />刷新</button>}>
      {error ? <div className="card-body"><ErrorBox error={error} /></div> : !data ? <Loading /> : (
        <>
          {data.hint && <div className="alert warn" style={{ margin: 16 }}><Icon name="alert" /><div style={{ whiteSpace: 'pre-wrap' }}>{data.hint}</div></div>}
          {data.items.length === 0 ? !data.hint && <Empty>暂无统计数据。</Empty> : (
            <div className="table-wrap">
              <table>
                <thead><tr><th>语句</th><th>调用次数</th><th>总耗时</th><th>平均</th><th>返回行</th></tr></thead>
                <tbody>
                  {data.items.map((q, i) => (
                    <tr key={i}>
                      <td className="mono" style={{ fontSize: 12, maxWidth: 560, overflowWrap: 'anywhere' }}>{q.query}</td>
                      <td className="mono">{q.calls}</td>
                      <td className="mono nowrap">{fmtMs(q.totalMs)}</td>
                      <td className="mono nowrap">{fmtMs(q.meanMs)}</td>
                      <td className="mono">{q.rows}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
          <div className="label" style={{ padding: '8px 16px' }}>{engine === 'postgres' ? '数据来自 pg_stat_statements。' : '数据来自 performance_schema 语句摘要。'}</div>
        </>
      )}
    </Card>
  )
}

const fmtMs = (ms: number) => (ms >= 1000 ? (ms / 1000).toFixed(2) + ' s' : ms.toFixed(1) + ' ms')

// ---------- import / export ----------

export function ImportExport({ id, engine, sql }: { id: string; engine: string; sql: boolean }) {
  const can = useCan()
  const toast = useToast()
  const openJob = useJobLog()
  const [db, setDb] = useState('')
  const [plain, setPlain] = useState(true)
  const [gzip, setGzip] = useState(true)
  const [file, setFile] = useState<File | null>(null)
  const [progress, setProgress] = useState<number | null>(null)
  const [confirm, setConfirm] = useState(false)
  const exportUrl = `/api/databases/${id}/export` + qs({ db, format: engine === 'postgres' && plain ? 'sql' : undefined, gzip: gzip ? 1 : undefined })
  const doImport = async () => {
    if (!file) return
    setProgress(0)
    try {
      const r = await upload<{ jobId: string }>('POST', `/databases/${id}/import` + qs({ db, name: file.name }), file, setProgress)
      openJob(r.jobId, `导入 ${file.name}`)
      setFile(null)
    } catch (e) {
      toast.err(e)
    } finally {
      setProgress(null)
    }
  }
  return (
    <div className="grid-2">
      <Card title="导出">
        <div className="col" style={{ gap: 12 }}>
          <Field label="库"><DbPicker id={id} value={db} onChange={setDb} /></Field>
          {engine === 'postgres' && (
            <div className="seg" role="group" aria-label="格式">
              <button className={plain ? 'on' : ''} onClick={() => setPlain(true)}>SQL 文本</button>
              <button className={!plain ? 'on' : ''} onClick={() => setPlain(false)}>pg_dump 自定义格式</button>
            </div>
          )}
          <label className="check"><input type="checkbox" checked={gzip} onChange={(e) => setGzip(e.target.checked)} />gzip 压缩</label>
          <div>{can.admin ? <a className={'btn btn-primary' + (db ? '' : ' disabled')} href={db ? exportUrl : undefined}><Icon name="download" size={16} />下载导出文件</a> : <span className="muted">只有管理员可以导出数据。</span>}</div>
          <span className="label">导出在服务器上实时生成并直接下载，不占用备份存储；导出操作会记录审计日志。</span>
        </div>
      </Card>
      <Card title="导入">
        {!sql ? <div className="muted">{engine} 不支持在线导入，请使用备份恢复。</div> : (
          <div className="col" style={{ gap: 12 }}>
            <Field label="导入到库"><DbPicker id={id} value={db} onChange={setDb} /></Field>
            <Field label="文件" div>
              <FilePicker file={file} onChange={setFile} accept=".sql,.dump,.gz,.pgdump,.backup" disabled={progress != null}
                hint={engine === 'postgres' ? '.sql 或 pg_dump 自定义格式（.dump），可再加 .gz' : '.sql，可再加 .gz'} />
            </Field>
            {progress != null && <div className="col" style={{ gap: 4 }}><div className="progress"><div style={{ width: progress + '%' }} /></div><span className="label">上传中 {progress}%</span></div>}
            <div className="alert warn" style={{ padding: '8px 12px' }}><Icon name="alert" />导入会在现有数据上执行文件中的语句（可能覆盖或冲突），建议先备份或导入到新库。</div>
            <div>{can.operator ? <button className="btn btn-primary" disabled={!file || !db || progress != null} onClick={() => setConfirm(true)}><Icon name="upload" size={16} />上传并导入</button> : <span className="muted">只读用户不能导入。</span>}</div>
          </div>
        )}
      </Card>
      {confirm && file && (
        <Confirm title="确认导入" confirmText="开始导入" onClose={() => setConfirm(false)} onConfirm={async () => { setConfirm(false); await doImport() }}>
          把 <b className="mono">{file.name}</b> 导入到库 <b className="mono">{db}</b>？导入在后台任务中执行，可以在任务日志中查看进度。
        </Confirm>
      )}
    </div>
  )
}
