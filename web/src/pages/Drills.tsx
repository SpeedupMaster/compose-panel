import { useState } from 'react'
import { ago, dateTime, del, get, post, put } from '../api'
import { useCan } from '../auth'
import { Card, Confirm, Empty, ErrorBox, Field, Icon, JobStatus, Loading, Modal, Select, useAction, useJobLog, useLoad, useToast } from '../components/ui'
import { MobileCard, ResponsiveTable, joinInline } from '../components/ResponsiveList'

type Drill = { id?: number; policyId: number; policy?: string; label?: string; kind?: string; schedule: string; checkSql: string; minRows: number; enabled: boolean; next?: number; lastRun?: DrillRun | null }
type DrillRun = { id: number; drillId: number; backupId: number; label: string; status: string; report: string; jobId: string; startedAt: number; finishedAt: number }

const kindText: Record<string, string> = { database: '数据库', volume: '数据卷', dir: '目录', panel: '面板数据' }

// Restore drills: periodically restore the latest backup into a throwaway
// container / volume and check that the data is usable.
export function Drills({ policies }: { policies: any[] }) {
  const can = useCan()
  const toast = useToast()
  const openJob = useJobLog()
  const { data, error, reload } = useLoad(() => get<{ drills: Drill[]; runs: DrillRun[] }>('/restore-drills'), [], 15000)
  const [edit, setEdit] = useState<Drill | null>(null)
  const [rm, setRm] = useState<Drill | null>(null)
  const [report, setReport] = useState<DrillRun | null>(null)
  if (!data) return error ? <ErrorBox error={error} /> : <Loading />
  const run = async (d: Drill) => {
    try {
      const r = await post(`/restore-drills/${d.id}/run`)
      openJob(r.jobId, `恢复演练 ${d.policy}`)
      setTimeout(reload, 1500)
    } catch (e) {
      toast.err(e)
    }
  }
  return (
    <>
      <div className="alert run">
        <Icon name="check" />
        <div>恢复演练会定期把策略的<b>最新一份成功备份</b>恢复到临时容器或临时数据卷（不联网、不影响线上），运行检查后自动清理，并记录报告。通过演练的备份会标记为「已演练」，失败时发送通知。</div>
      </div>
      <Card
        title="演练计划"
        pad={false}
        actions={can.operator && <button className="btn btn-sm btn-primary" disabled={policies.length === 0} onClick={() => setEdit({ policyId: policies[0]?.id, schedule: '0 5 * * 0', checkSql: '', minRows: 0, enabled: true })}><Icon name="plus" size={16} />新建演练</button>}
      >
        {data.drills.length === 0 ? <Empty>{policies.length === 0 ? '先创建备份策略，再为它设置恢复演练。' : '还没有恢复演练。建议为每个重要数据库每周演练一次。'}</Empty> : (
          <ResponsiveTable
            table={
              <div className="table-wrap">
                <table>
                  <thead><tr><th>备份策略</th><th>类型</th><th>计划</th><th>检查</th><th>上次结果</th><th>下次</th><th /></tr></thead>
                  <tbody>
                    {data.drills.map((d) => (
                      <tr key={d.id}>
                        <td style={{ fontWeight: 600 }}>{d.policy}{!d.enabled && <span className="badge off" style={{ marginLeft: 6 }}>停用</span>}<div className="label">{d.label}</div></td>
                        <td>{kindText[d.kind || ''] || d.kind}</td>
                        <td className="mono">{d.schedule}</td>
                        <td className="mono ellipsis" style={{ maxWidth: 220, fontSize: 12 }} title={d.checkSql}>{d.checkSql || <span className="muted">默认</span>}{d.minRows > 0 && <div className="label">≥ {d.minRows}</div>}</td>
                        <td className="nowrap">{d.lastRun ? <button className="link-btn" style={{ textDecoration: 'none' }} onClick={() => setReport(d.lastRun!)}><JobStatus s={d.lastRun.status} /> <span className="label">{ago(d.lastRun.startedAt)}</span></button> : <span className="muted">—</span>}</td>
                        <td className="muted nowrap">{d.enabled && d.next ? ago(d.next) : '—'}</td>
                        <td>
                          {can.operator && (
                            <div className="row" style={{ justifyContent: 'flex-end' }}>
                              <button className="btn btn-sm" onClick={() => run(d)}><Icon name="play" size={14} />立即演练</button>
                              <button className="btn btn-sm btn-ghost btn-icon" aria-label="编辑" onClick={() => setEdit({ ...d })}><Icon name="edit" size={16} /></button>
                              <button className="btn btn-sm btn-ghost btn-icon" aria-label="删除" onClick={() => setRm(d)}><Icon name="trash" size={16} /></button>
                            </div>
                          )}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            }
            cards={data.drills.map((d) => (
              <MobileCard
                key={d.id}
                title={d.policy || `策略 #${d.policyId}`}
                sub={d.label}
                badges={
                  <>
                    {!d.enabled && <span className="badge off">停用</span>}
                    {d.lastRun && <JobStatus s={d.lastRun.status} />}
                  </>
                }
                meta={[
                  { label: '类型', value: kindText[d.kind || ''] || d.kind },
                  { label: '计划', value: d.schedule, mono: true },
                  { label: '检查', value: (d.checkSql || '默认') + (d.minRows > 0 ? ` (≥ ${d.minRows})` : ''), mono: Boolean(d.checkSql), full: true },
                  { label: '时间', value: joinInline([d.lastRun && `上次 ${ago(d.lastRun.startedAt)}`, d.enabled && d.next && `下次 ${ago(d.next)}`]) || '—', full: true },
                ]}
                actions={
                  can.operator ? (
                    <>
                      <button className="btn btn-sm" onClick={() => run(d)}><Icon name="play" size={14} />立即演练</button>
                      {d.lastRun && <button className="btn btn-sm btn-ghost" onClick={() => setReport(d.lastRun!)}>上次报告</button>}
                      <button className="btn btn-sm btn-ghost btn-icon" aria-label="编辑" onClick={() => setEdit({ ...d })}><Icon name="edit" size={16} /></button>
                      <button className="btn btn-sm btn-ghost btn-icon" aria-label="删除" onClick={() => setRm(d)}><Icon name="trash" size={16} /></button>
                    </>
                  ) : undefined
                }
              />
            ))}
          />
        )}
      </Card>
      <Card title="演练记录" pad={false}>
        {data.runs.length === 0 ? <Empty>还没有演练记录。</Empty> : (
          <ResponsiveTable
            table={
              <div className="table-wrap">
                <table>
                  <thead><tr><th>备份</th><th>结果</th><th>开始</th><th>耗时</th><th>报告</th><th /></tr></thead>
                  <tbody>
                    {data.runs.map((r) => (
                      <tr key={r.id}>
                        <td>{r.label}<div className="label mono">备份 #{r.backupId || '—'}</div></td>
                        <td><JobStatus s={r.status} /></td>
                        <td className="muted nowrap" title={dateTime(r.startedAt)}>{ago(r.startedAt)}</td>
                        <td className="muted nowrap">{r.finishedAt ? Math.max(0, r.finishedAt - r.startedAt) + ' 秒' : '—'}</td>
                        <td className="ellipsis" style={{ maxWidth: 320, fontSize: 13 }} title={r.report}>{(r.report || '').split('\n').filter(Boolean).slice(-1)[0] || '—'}</td>
                        <td>
                          <div className="row" style={{ justifyContent: 'flex-end' }}>
                            <button className="btn btn-sm" onClick={() => setReport(r)}>报告</button>
                            {r.jobId && <button className="btn btn-sm btn-ghost" onClick={() => openJob(r.jobId, '恢复演练')}>日志</button>}
                          </div>
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            }
            cards={data.runs.map((r) => {
              const tail = (r.report || '').split('\n').filter(Boolean).slice(-1)[0]
              return (
                <MobileCard
                  key={r.id}
                  title={r.label}
                  sub={<span className="mono">备份 #{r.backupId || '—'}</span>}
                  badges={<JobStatus s={r.status} />}
                  extra={tail ? <div className="label" style={{ overflowWrap: 'anywhere' }}>{tail}</div> : undefined}
                  meta={[
                    { label: '开始', value: ago(r.startedAt) },
                    { label: '耗时', value: r.finishedAt ? Math.max(0, r.finishedAt - r.startedAt) + ' 秒' : '—' },
                  ]}
                  actions={
                    <>
                      <button className="btn btn-sm" onClick={() => setReport(r)}>报告</button>
                      {r.jobId && <button className="btn btn-sm btn-ghost" onClick={() => openJob(r.jobId, '恢复演练')}>日志</button>}
                    </>
                  }
                />
              )
            })}
          />
        )}
      </Card>
      {edit && <DrillModal d={edit} policies={policies} onClose={() => setEdit(null)} onSaved={() => { setEdit(null); reload() }} />}
      {rm && <Confirm title="删除恢复演练" danger confirmText="删除" onClose={() => setRm(null)} onConfirm={async () => { await del(`/restore-drills/${rm.id}`); reload() }}>只删除演练计划，备份不受影响。</Confirm>}
      {report && (
        <Modal title={`演练报告 · ${report.label}`} wide onClose={() => setReport(null)} footer={<>{report.jobId && <button className="btn" onClick={() => openJob(report.jobId, '恢复演练')}>完整日志</button>}<button className="btn btn-primary" onClick={() => setReport(null)}>关闭</button></>}>
          <div className="row"><JobStatus s={report.status} /><span className="label">{dateTime(report.startedAt)}{report.finishedAt ? ` · 用时 ${Math.max(0, report.finishedAt - report.startedAt)} 秒` : ''}</span></div>
          <pre className="log" style={{ margin: 0, borderRadius: 6, maxHeight: 420 }}>{report.report || '（没有报告）'}</pre>
        </Modal>
      )}
    </>
  )
}

function DrillModal({ d, policies, onClose, onSaved }: { d: Drill; policies: any[]; onClose: () => void; onSaved: () => void }) {
  const [f, setF] = useState<Drill>(d)
  const { busy, run } = useAction()
  const pol = policies.find((p) => p.id === f.policyId)
  const kind = pol?.source?.kind
  const save = async () => {
    const r = await run('save', () => (f.id ? put(`/restore-drills/${f.id}`, f) : post('/restore-drills', f)), '已保存')
    if (r) onSaved()
  }
  return (
    <Modal title={f.id ? '编辑恢复演练' : '新建恢复演练'} onClose={onClose} footer={<><button className="btn" onClick={onClose}>取消</button><button className="btn btn-primary" disabled={!!busy || !f.policyId} onClick={save}>保存</button></>}>
      <Field label="备份策略">
        <Select value={f.policyId || ''} onChange={(v) => setF({ ...f, policyId: Number(v) })}>
          {policies.map((p) => <option key={p.id} value={p.id}>{p.name}（{kindText[p.source.kind] || p.source.kind}）</option>)}
        </Select>
      </Field>
      <Field label="计划（cron）" hint="默认每周日 05:00；演练会占用一些 CPU 和磁盘，建议避开业务高峰">
        <input className="input mono" value={f.schedule} onChange={(e) => setF({ ...f, schedule: e.target.value })} />
      </Field>
      {kind === 'database' && (
        <>
          <Field label="检查语句（可选）" hint="返回一个数字，例如 SELECT count(*) FROM users；MongoDB 填 JS 表达式，如 db.users.countDocuments()。留空默认统计表数量（Redis 为键数量）">
            <textarea className="textarea code" rows={3} value={f.checkSql} onChange={(e) => setF({ ...f, checkSql: e.target.value })} />
          </Field>
          <Field label="最小值" hint="检查结果小于该值则判定失败"><input className="input" type="number" min={0} style={{ maxWidth: 160 }} value={f.minRows} onChange={(e) => setF({ ...f, minRows: Number(e.target.value) })} /></Field>
        </>
      )}
      {kind && kind !== 'database' && (
        <Field label="最少文件数" hint="恢复出的文件数量小于该值则判定失败（0 表示只要求非空）"><input className="input" type="number" min={0} style={{ maxWidth: 160 }} value={f.minRows} onChange={(e) => setF({ ...f, minRows: Number(e.target.value) })} /></Field>
      )}
      <label className="check"><input type="checkbox" checked={f.enabled} onChange={(e) => setF({ ...f, enabled: e.target.checked })} />启用</label>
    </Modal>
  )
}
