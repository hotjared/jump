import { useEffect, useState } from 'react'
import type { WorkspaceSession } from './SessionWorkspace'
import './dashboard.css'

export type DashboardDevice = {
  id: string; hostname: string; display_name: string | null; online: boolean;
  identity_state: string;
  agent_update: { update_available: boolean; current_version: string; latest_version: string | null;
    update_state: { state: string; failure_reason: string | null; completed_at?: string | null } | null };
}
export type DashboardEvent = {
  id: string; event_type: string; created_at: string; device_id: string | null;
  detail?: Record<string, unknown> | null;
}

const labels: Record<string, string> = {
  ssh_session_started: 'SSH session started', rdp_session_started: 'RDP session started',
  ssh_session_ended: 'SSH session closed', rdp_session_ended: 'RDP session closed',
  ssh_session_failed: 'SSH session failed', rdp_session_failed: 'RDP session failed',
  file_upload_completed: 'Uploaded', file_download_completed: 'Downloaded',
  file_upload_failed: 'Upload failed', file_download_failed: 'Download failed',
  agent_update_completed: 'Agent updated', agent_update_failed: 'Agent update failed',
  device_enrolled: 'Device enrolled', agent_identity_revoked: 'Device revoked',
}
const recent = (date: string) => Date.now() - new Date(date).getTime() < 24 * 60 * 60 * 1000
const reason = (value: unknown) => typeof value === 'string' && /^[a-z][a-z0-9_]{0,79}$/.test(value)
  ? value.replaceAll('_', ' ') : null
const filename = (value: unknown) => typeof value === 'string' && value.length <= 180 && !/[\\/\x00-\x1f]/.test(value)
  ? value : null
const time = (value: string) => new Date(value).toLocaleString()

export default function Dashboard({ devices, sessions, events, open }: {
  devices: DashboardDevice[]; sessions: WorkspaceSession[]; events: DashboardEvent[]; open: (id: string) => void;
}) {
  const [, update] = useState(0)
  useEffect(() => {
    const detach = sessions.map(session => session.subscribe(() => update(n => n + 1)))
    return () => detach.forEach(unsubscribe => unsubscribe())
  }, [sessions])
  const names = new Map(devices.map(d => [d.id, d.display_name || d.hostname]))
  const meaningful = events.filter(e => labels[e.event_type]).sort((a, b) =>
    new Date(b.created_at).getTime() - new Date(a.created_at).getTime()).slice(0, 10)
  const issues: { key: string; title: string; device: string; detail?: string; date?: string }[] = []
  for (const d of devices) {
    const name = d.display_name || d.hostname
    const state = d.agent_update?.update_state
    if (state?.state === 'failed') issues.push({ key: `update-failed-${d.id}`, title: 'Agent update failed',
      device: name, detail: reason(state.failure_reason) || undefined, date: state.completed_at || undefined })
    if (d.agent_update?.update_available && d.identity_state === 'active') issues.push({
      key: `update-${d.id}`, title: 'Agent update available', device: name,
      detail: `${d.agent_update.current_version || 'Unknown'} → ${d.agent_update.latest_version || 'Unknown'}`,
    })
    if (d.identity_state === 'revoked') issues.push({ key: `revoked-${d.id}`, title: 'Device revoked', device: name })
  }
  for (const e of events) {
    if (!recent(e.created_at)) continue
    if (!['file_upload_failed', 'file_download_failed', 'ssh_session_failed', 'rdp_session_failed'].includes(e.event_type)) continue
    const direction = e.event_type.includes('upload') ? 'Upload' : 'Download'
    issues.push({ key: e.id, title: e.event_type.startsWith('file_') ? `${direction} failed` : labels[e.event_type],
      device: names.get(e.device_id || '') || 'Unknown device',
      detail: [filename(e.detail?.filename), reason(e.detail?.reason)].filter(Boolean).join(' · ') || undefined,
      date: e.created_at })
  }
  issues.sort((a, b) => (b.date || '').localeCompare(a.date || ''))

  return <div className="dashboard">
    <div className="heading"><div><p className="eyebrow">OPERATIONS</p><h1>Dashboard</h1><p>What needs attention in Jump right now.</p></div></div>
    <div className="dashboard-stats">
      <div><small>Active Sessions</small><strong>{sessions.length}</strong></div>
      <div><small>Devices Online</small><strong>{devices.filter(d => d.online).length} / {devices.length}</strong></div>
      <div><small>Agent Updates</small><strong>{devices.filter(d => d.agent_update?.update_available && d.identity_state === 'active').length}</strong></div>
      <div><small>Issues</small><strong>{issues.length}</strong></div>
    </div>
    <section className="panel dashboard-panel" aria-label="Needs Attention"><h2>Needs Attention</h2>
      {issues.length ? issues.map(item => <div className="dashboard-row" key={item.key}>
        <span className="dashboard-issue-mark" aria-hidden="true">!</span><div className="dashboard-row-main"><strong>{item.title}</strong><span>{item.device}</span>{item.detail && <small>{item.detail}</small>}</div>
        {item.date && <time dateTime={item.date}>{time(item.date)}</time>}
      </div>) : <div className="dashboard-empty">Nothing needs attention.</div>}
    </section>
    <section className="panel dashboard-panel" aria-label="Active Sessions"><h2>Active Sessions</h2>
      {sessions.length ? sessions.map(session => <div className="dashboard-row" key={session.id}>
        <div className="dashboard-row-main"><strong>{session.name}</strong><span>{session.protocol} · {session.state}</span></div>
        <button className="button" onClick={() => open(session.id)} aria-label={`Open ${session.name} ${session.protocol} session`}>Open</button>
      </div>) : <div className="dashboard-empty">No active sessions.</div>}
    </section>
    <section className="panel dashboard-panel" aria-label="Recent Activity"><h2>Recent Activity</h2>
      {meaningful.length ? meaningful.map(e => {
        const file = filename(e.detail?.filename)
        const label = labels[e.event_type] + (file && e.event_type.startsWith('file_') ? ` ${file}` : '')
        const name = names.get(e.device_id || '')
        return <div className="dashboard-row" key={e.id}><div className="dashboard-row-main">
          <span>{label}{name ? ` on ${name}` : ''}</span></div><time dateTime={e.created_at}>{time(e.created_at)}</time></div>
      }) : <div className="dashboard-empty">No recent activity to show.</div>}
    </section>
  </div>
}
