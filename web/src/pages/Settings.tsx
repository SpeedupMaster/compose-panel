import { useEffect, useState } from 'react'
import { useLocation, useNavigate } from 'react-router-dom'
import { ago, bytes, dateTime, del, duration, get, post, put } from '../api'
import { useAuth, useCan } from '../auth'
import { Card, Confirm, Empty, ErrorBox, Field, Icon, Loading, Modal, Page, SecretBox, Tabs, useJobLog, useLoad, useToast } from '../components/ui'

type Tab = 'panel' | 'account' | 'system' | 'import'

export default function Settings() {
  const can = useCan()
  const loc = useLocation()
  const [tab, setTab] = useState<Tab>(loc.hash === '#import' && can.admin ? 'import' : can.admin ? 'panel' : 'account')
  const tabs: [Tab, string][] = can.admin ? [['panel', '面板设置'], ['account', '我的账号'], ['system', '系统信息'], ['import', '导入项目']] : [['account', '我的账号'], ['system', '系统信息']]
  return (
    <Page title="设置">
      <Tabs tabs={tabs} value={tab} onChange={setTab} />
      {tab === 'panel' && <PanelSettings />}
      {tab === 'account' && <Account />}
      {tab === 'system' && <SystemInfo />}
      {tab === 'import' && <Import />}
    </Page>
  )
}

function PanelSettings() {
  const { data, error, reload } = useLoad(() => get<Record<string, string>>('/settings'), [])
  const toast = useToast()
  const [f, setF] = useState<Record<string, string>>({})
  useEffect(() => {
    if (data) setF(data)
  }, [data])
  if (!data) return error ? <ErrorBox error={error} /> : <Loading />
  const set = (k: string) => (e: React.ChangeEvent<HTMLInputElement | HTMLSelectElement>) => setF({ ...f, [k]: e.target.value })
  const save = async () => {
    try {
      const r = await put('/settings', f)
      if (r.warning) toast.err(r.warning)
      else toast.ok('设置已保存')
      reload()
    } catch (e) {
      toast.err(e)
    }
  }
  return (
    <>
      <Card title="访问">
        <div className="form-grid">
          <Field label="面板域名" hint="设置后 Caddy 会为面板签发证书，通过 https://域名 访问"><input className="input mono" value={f.panel_domain || ''} onChange={set('panel_domain')} placeholder="panel.example.com" /></Field>
          <Field label="ACME 邮箱" hint="Let's Encrypt 证书到期提醒"><input className="input" type="email" value={f.acme_email || ''} onChange={set('acme_email')} /></Field>
          <Field label="服务器公网 IP" hint="用于检查域名解析是否指向本机"><input className="input mono" value={f.public_ip || ''} onChange={set('public_ip')} /></Field>
          <Field label="登录有效期（小时）"><input className="input" type="number" min={1} value={f.session_ttl_hours || ''} onChange={set('session_ttl_hours')} /></Field>
        </div>
        <label className="check" style={{ marginTop: 12 }}><input type="checkbox" checked={f.secure_headers === '1'} onChange={(e) => setF({ ...f, secure_headers: e.target.checked ? '1' : '0' })} />为所有站点添加安全响应头（X-Content-Type-Options 等）</label>
      </Card>
      <Card title="计划任务与告警">
        <div className="form-grid">
          <Field label="时区"><input className="input mono" value={f.timezone || ''} onChange={set('timezone')} placeholder="Asia/Shanghai" /></Field>
          <Field label="镜像清理计划" hint="cron，留空关闭"><input className="input mono" value={f.image_prune_cron || ''} onChange={set('image_prune_cron')} /></Field>
          <Field label="面板自身备份计划" hint="cron，保留最近 14 份"><input className="input mono" value={f.panel_backup_cron || ''} onChange={set('panel_backup_cron')} /></Field>
          <Field label="磁盘告警阈值（%）"><input className="input" type="number" min={50} max={99} value={f.disk_alert_percent || ''} onChange={set('disk_alert_percent')} /></Field>
          <Field label="证书提前告警（天）"><input className="input" type="number" min={1} value={f.cert_alert_days || ''} onChange={set('cert_alert_days')} /></Field>
        </div>
      </Card>
      <div className="row" style={{ justifyContent: 'flex-end' }}><button className="btn btn-primary" onClick={save}>保存设置</button></div>
    </>
  )
}

function Account() {
  const { me, refresh } = useAuth()
  const toast = useToast()
  const [pw, setPw] = useState({ current: '', new: '', confirm: '' })
  const [totp, setTotp] = useState<{ secret: string; uri: string } | null>(null)
  const [code, setCode] = useState('')
  const [codes, setCodes] = useState<string[] | null>(null)
  const [disable, setDisable] = useState(false)
  const [disablePw, setDisablePw] = useState('')
  const { data: sessions, reload: reloadSessions } = useLoad(() => get<any[]>('/me/sessions'), [])
  const u = me!.user
  const changePw = async () => {
    if (pw.new !== pw.confirm) return toast.err('两次输入的新密码不一致')
    try {
      await post('/me/password', { current: pw.current, new: pw.new })
      toast.ok('密码已修改，其它会话已注销')
      setPw({ current: '', new: '', confirm: '' })
      reloadSessions()
    } catch (e) {
      toast.err(e)
    }
  }
  return (
    <>
      <div className="grid-2">
        <Card title="修改密码">
          <div className="col" style={{ gap: 12 }}>
            <Field label="当前密码"><input className="input" type="password" value={pw.current} onChange={(e) => setPw({ ...pw, current: e.target.value })} autoComplete="current-password" /></Field>
            <Field label="新密码" hint="至少 10 位"><input className="input" type="password" value={pw.new} onChange={(e) => setPw({ ...pw, new: e.target.value })} autoComplete="new-password" /></Field>
            <Field label="确认新密码"><input className="input" type="password" value={pw.confirm} onChange={(e) => setPw({ ...pw, confirm: e.target.value })} autoComplete="new-password" /></Field>
            <div><button className="btn btn-primary" disabled={!pw.current || pw.new.length < 10} onClick={changePw}>修改密码</button></div>
          </div>
        </Card>
        <Card title="两步验证（TOTP）">
          {u.totp ? (
            <div className="col" style={{ gap: 12 }}>
              <div className="alert ok"><Icon name="check" />已开启。登录时需要输入验证器中的动态码。</div>
              <div><button className="btn btn-danger" onClick={() => setDisable(true)}>关闭两步验证</button></div>
            </div>
          ) : totp ? (
            <div className="col" style={{ gap: 12 }}>
              <p style={{ margin: 0 }}>在验证器（Google Authenticator、1Password、Bitwarden 等）中手动添加以下密钥，或在手机上打开链接：</p>
              <SecretBox label="密钥" value={totp.secret} />
              <a className="mono" style={{ fontSize: 12, overflowWrap: 'anywhere' }} href={totp.uri}>{totp.uri}</a>
              <Field label="输入 6 位动态码确认"><input className="input mono" inputMode="numeric" value={code} onChange={(e) => setCode(e.target.value)} /></Field>
              <div className="row">
                <button className="btn btn-primary" onClick={async () => { try { const r = await post('/me/totp/enable', { code: code.trim() }); setCodes(r.recoveryCodes); setTotp(null); refresh() } catch (e) { toast.err(e) } }}>开启</button>
                <button className="btn" onClick={() => setTotp(null)}>取消</button>
              </div>
            </div>
          ) : (
            <div className="col" style={{ gap: 12 }}>
              <div className="alert warn"><Icon name="alert" />尚未开启。面板可以控制服务器上的所有容器，强烈建议开启。</div>
              <div><button className="btn btn-primary" onClick={async () => { try { setTotp(await post('/me/totp/setup')) } catch (e) { toast.err(e) } }}>开始设置</button></div>
            </div>
          )}
        </Card>
      </div>
      <Card title="登录会话" pad={false}>
        {!sessions ? <Loading /> : sessions.length === 0 ? <Empty>没有会话。</Empty> : (
          <div className="table-wrap">
            <table>
              <thead><tr><th>设备</th><th>IP</th><th>登录</th><th>最近活动</th><th></th></tr></thead>
              <tbody>
                {sessions.map((s) => (
                  <tr key={s.id}>
                    <td className="ellipsis" style={{ maxWidth: 360 }} title={s.userAgent}>{s.userAgent || '—'}{s.current && <span className="badge run" style={{ marginLeft: 6 }}>当前</span>}</td>
                    <td className="mono">{s.ip}</td>
                    <td className="muted nowrap">{dateTime(s.createdAt)}</td>
                    <td className="muted nowrap">{ago(s.lastSeenAt)}</td>
                    <td>{!s.current && <button className="btn btn-sm" onClick={async () => { try { await del(`/me/sessions/${s.id}`); reloadSessions() } catch (e) { toast.err(e) } }}>注销</button>}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Card>
      {codes && (
        <Modal title="恢复码" onClose={() => setCodes(null)} footer={<button className="btn btn-primary" onClick={() => setCodes(null)}>我已保存</button>}>
          <p style={{ margin: 0 }}>两步验证已开启。请保存下面的恢复码，手机丢失时每个可以代替动态码使用一次。只显示这一次。</p>
          <SecretBox value={codes.join('  ')} />
        </Modal>
      )}
      {disable && (
        <Modal title="关闭两步验证" onClose={() => setDisable(false)} footer={<><button className="btn" onClick={() => setDisable(false)}>取消</button><button className="btn btn-danger solid" onClick={async () => { try { await post('/me/totp/disable', { password: disablePw }); setDisable(false); refresh(); toast.ok('已关闭两步验证') } catch (e) { toast.err(e) } }}>关闭</button></>}>
          <Field label="输入当前密码确认"><input className="input" type="password" value={disablePw} onChange={(e) => setDisablePw(e.target.value)} autoComplete="current-password" /></Field>
        </Modal>
      )}
    </>
  )
}

function SystemInfo() {
  const { data, error } = useLoad(() => get<any>('/system/info'), [])
  const can = useCan()
  const openJob = useJobLog()
  const toast = useToast()
  const [confirm, setConfirm] = useState(false)
  if (!data) return error ? <ErrorBox error={error} /> : <Loading />
  return (
    <div className="grid-2">
      <Card title="面板">
        <dl className="kv">
          <dt>版本</dt><dd className="mono">{data.version}（{data.go}）</dd>
          <dt>已运行</dt><dd>{duration(Date.now() / 1000 - data.startedAt)}</dd>
          <dt>数据目录</dt><dd className="mono">{data.dataDir}</dd>
          <dt>项目目录</dt><dd className="mono">{data.projectsDir}</dd>
          <dt>备份目录</dt><dd className="mono">{data.backupsDir}</dd>
          <dt>数据库大小</dt><dd>{bytes(data.dbSize)}</dd>
          <dt>面板自身备份</dt><dd>{data.panelBackups} 份{data.lastPanelBackup ? `，最近 ${ago(data.lastPanelBackup)}` : ''}</dd>
          <dt>Caddy</dt><dd>{data.caddy === 'ok' ? <span className="badge ok">已连接</span> : <span className="badge err" title={data.caddy}>无法连接</span>}</dd>
        </dl>
        {can.admin && <div style={{ marginTop: 16 }}><button className="btn" onClick={() => setConfirm(true)}>立即备份面板数据</button></div>}
      </Card>
      <Card title="主机与 Docker">
        <dl className="kv">
          <dt>主机名</dt><dd>{data.hostname || '—'}</dd>
          <dt>系统</dt><dd>{data.osName || '—'}<div className="label">{data.os}</div></dd>
          <dt>CPU / 内存</dt><dd>{data.cpus} 核 · {bytes(data.memTotal)}</dd>
          <dt>Docker</dt><dd>{data.dockerVersion ? `${data.dockerVersion}（API ${data.dockerApi}）` : <span className="badge err">{data.dockerError}</span>}</dd>
          <dt>代理网络</dt><dd className="mono">{data.proxyNetwork}</dd>
          <dt>数据库网络</dt><dd className="mono">{data.dbNetwork}</dd>
        </dl>
      </Card>
      {confirm && <Confirm title="备份面板数据" confirmText="开始" onClose={() => setConfirm(false)} onConfirm={async () => { try { const r = await post('/system/panel-backup'); openJob(r.jobId, '面板备份') } catch (e) { toast.err(e) } }}>把 panel.db 快照保存到备份目录下的 _panel 目录。</Confirm>}
    </div>
  )
}

function Import() {
  const { data, error, reload } = useLoad(() => get<any[]>('/import/scan'), [])
  const toast = useToast()
  const nav = useNavigate()
  if (!data) return error ? <ErrorBox error={error} /> : <Loading />
  const list = data.filter((x) => !x.managed)
  return (
    <Card title="导入已有的 Compose 项目" actions={<button className="btn btn-sm" onClick={reload}><Icon name="refresh" size={16} />重新扫描</button>} pad={false}>
      <p className="label" style={{ padding: '12px 16px 0', margin: 0 }}>扫描主机上由 docker compose 启动、但尚未被面板管理的项目。导入后面板会在原目录中执行 compose 命令，不会移动文件。项目目录需要挂载进面板容器（同路径）。</p>
      {list.length === 0 ? <Empty>没有发现可导入的项目。</Empty> : (
        <div className="table-wrap">
          <table>
            <thead><tr><th>项目</th><th>目录</th><th>服务</th><th>容器</th><th></th></tr></thead>
            <tbody>
              {list.map((x) => (
                <tr key={x.name}>
                  <td style={{ fontWeight: 600 }}>{x.name}</td>
                  <td className="mono" style={{ fontSize: 12 }}>{x.workdir}<div className="label">{x.files?.join(', ')}</div></td>
                  <td>{x.services?.join(', ')}</td>
                  <td>{x.running}/{x.containers}</td>
                  <td><button className="btn btn-sm btn-primary" onClick={async () => { try { const r = await post('/import', { name: x.name, workdir: x.workdir, files: x.files }); toast.ok('已导入'); nav(`/projects/${r.id}`) } catch (e) { toast.err(e) } }}>导入</button></td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </Card>
  )
}
