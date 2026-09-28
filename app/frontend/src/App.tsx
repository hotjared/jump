import { lazy, Suspense, useCallback, useEffect, useMemo, useState } from 'react'
import { agentFilename, downloadFor, enrollmentCommand, type AgentDownloads, type Platform } from './agent-downloads'
import { canDeleteDevice, deletionConfirmed, deletionName, deviceDeleteMethod } from './device-actions'
import AgentUpdatePanel, { type UpdateInfo } from './AgentUpdatePanel'
import { SshSession } from './ssh-session'
import { RdpSession } from './rdp-session'
import type { WorkspaceSession } from './SessionWorkspace'
import QuickConnect, { type QuickPreference } from './QuickConnect'
import Notice, { type NoticeMessage } from './Notice'
import Dashboard, { type DashboardEvent } from './Dashboard'
const TerminalPanel = lazy(() => import('./TerminalPanel'))
const RemotePanel = lazy(() => import('./RemotePanel'))
const SessionWorkspace = lazy(() => import('./SessionWorkspace'))
const FilesPanel = lazy(() => import('./FilesPanel'))
const CredentialsPage = lazy(() => import('./CredentialsPage'))

type Named = { id: string; name: string }
type Device = {
  id: string; device_uuid: string; hostname: string; display_name: string | null;
  os_family: string; os_version: string; architecture: string; agent_version: string;
  capabilities: string[]; addresses: string[]; primary_ip: string | null;
  current_user: string | null; group: Named | null; tags: Named[];
  online: boolean; identity_state: "active" | "revoked" | "none"; last_seen_at: string | null; enrolled_at: string; ssh_host_key: string | null;
  agent_update: UpdateInfo;
}
type User = { id: string; email: string; display_name: string; role: string; csrf: string }
type SystemInfo = { server_version: string; target_agent_version: string | null }
type Event = DashboardEvent
type Page = 'Dashboard' | 'Devices' | 'Credentials' | 'Support Links' | 'Audit Log' | 'Settings'
const nav: { page: Page; icon: string }[] = [
  { page: 'Dashboard', icon: '◫' }, { page: 'Devices', icon: '▤' },
  { page: 'Credentials', icon: '◇' },
  { page: 'Support Links', icon: '↗' }, { page: 'Audit Log', icon: '≡' },
  { page: 'Settings', icon: '⚙' },
]
const formatDate = (value: string | null) => value ? new Date(value).toLocaleString() : 'Never'

async function get<T>(url: string): Promise<T> {
  const result = await fetch(url, { credentials: 'same-origin' })
  if (!result.ok) throw new Error(`Request failed: ${result.status}`)
  return result.json() as Promise<T>
}

export function filterDevices(devices: Device[], query: string, status: string, os: string, group: string, tag: string) {
  const q = query.toLowerCase().trim()
  return devices.filter(d =>
    (!q || [d.hostname, d.display_name, d.current_user, d.primary_ip].some(v => v?.toLowerCase().includes(q))) &&
    (status === 'all' || (status === 'online') === d.online) &&
    (os === 'all' || d.os_family === os) &&
    (group === 'all' || d.group?.id === group) &&
    (tag === 'all' || d.tags.some(t => t.id === tag)))
}

export default function App() {
  const [user, setUser] = useState<User | null>(null)
  const [authPending, setAuthPending] = useState(true)
  const [page, setPage] = useState<Page>('Devices')
  const [sessions, setSessions] = useState<WorkspaceSession[]>([])
  const [quickPreferences, setQuickPreferences] = useState<QuickPreference[]>([])
  const [notice, setNotice] = useState<NoticeMessage | null>(null)
  const dismissNotice = useCallback(() => setNotice(null), [])
  const [activeSessionId, setActiveSessionId] = useState<string | null>(null)
  const [devices, setDevices] = useState<Device[]>([])
  const [groups, setGroups] = useState<Named[]>([])
  const [tags, setTags] = useState<Named[]>([])
  const [systemInfo, setSystemInfo] = useState<SystemInfo | null>(null)
  const [systemInfoError, setSystemInfoError] = useState(false)
  const [events, setEvents] = useState<Event[]>([])
  const [selected, setSelected] = useState<string | null>(null)
  const [tab, setTab] = useState('Overview')
  const [query, setQuery] = useState('')
  const [status, setStatus] = useState('all')
  const [os, setOs] = useState('all')
  const [group, setGroup] = useState('all')
  const [tag, setTag] = useState('all')
  const [enrolling, setEnrolling] = useState(false)
  const [platform, setPlatform] = useState<Platform>('linux')
  const [issued, setIssued] = useState<{ token: string; expires_at: string; agent_url: string } | null>(null)
  const [agentRelease, setAgentRelease] = useState<AgentDownloads | null>(null)
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const [newGroup, setNewGroup] = useState('')
  const [newTag, setNewTag] = useState('')
  const [editing, setEditing] = useState<string | null>(null)
  const [editValue, setEditValue] = useState('')

  async function refresh(admin: boolean) {
    const [d, g, t] = await Promise.all([
      get<Device[]>('/api/devices'), get<Named[]>('/api/groups'), get<Named[]>('/api/tags'),
    ])
    setDevices(d); setGroups(g); setTags(t)
    if (admin) setEvents(await get<Event[]>('/api/audit'))
  }
  useEffect(() => {
    get<User>('/api/me').then(u => {
      setUser(u); setAuthPending(false)
      if (u.role === 'admin') get<QuickPreference[]>('/api/quick-connect-preferences').then(setQuickPreferences).catch(() => setError('Could not load Quick Connect settings.'))
      return refresh(u.role === 'admin')
    }).catch(() => setAuthPending(false))
  }, [])
  useEffect(() => {
    if (!user) return
    const timer = setInterval(() => { refresh(user.role === 'admin').catch(() => {}) }, 15000)
    return () => clearInterval(timer)
  }, [user])
  useEffect(() => {
    if (!enrolling || user?.role !== 'admin') return
    get<AgentDownloads>('/api/agent-downloads').then(setAgentRelease).catch(() => setError('Could not load agent downloads.'))
  }, [enrolling, user])
  useEffect(() => {
    if (page !== 'Settings' || !user) return
    let active = true
    get<SystemInfo>('/api/system-info')
      .then(info => { if (active) { setSystemInfo(info); setSystemInfoError(false) } })
      .catch(() => { if (active) setSystemInfoError(true) })
    return () => { active = false }
  }, [page, user])
  const visible = useMemo(() => filterDevices(devices, query, status, os, group, tag),
    [devices, query, status, os, group, tag])
  const device = devices.find(d => d.id === selected)
  const activeSession = sessions.find(session => session.id === activeSessionId)

  function openSession(session: WorkspaceSession, { activate = true } = {}) {
    // A disconnected tab may be replaced by a fresh connection to the same device.
    sessions.filter(old => old.deviceId === session.deviceId).forEach(old => old.disconnect())
    setSessions(previous => [...previous.filter(old => old.deviceId !== session.deviceId), session])
    if (activate) { setActiveSessionId(session.id); setSelected(null) }
  }

  function closeSession(id: string) {
    const index = sessions.findIndex(session => session.id === id)
    sessions[index]?.disconnect()
    const remaining = sessions.filter(session => session.id !== id)
    setSessions(remaining)
    if (activeSessionId === id) setActiveSessionId(remaining[index]?.id || remaining[index - 1]?.id || null)
  }

  async function mutate<T>(url: string, method: string, body?: unknown): Promise<T> {
    const response = await fetch(url, {
      method, credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': user!.csrf },
      body: body ? JSON.stringify(body) : undefined,
    })
    if (!response.ok) {
      let detail = `Request failed: ${response.status}`
      try {
        const payload = await response.json() as { detail?: string }
        if (payload.detail) detail = payload.detail
      } catch {}
      throw new Error(detail)
    }
    return response.json() as Promise<T>
  }
  async function action(task: () => Promise<void>) {
    setBusy(true); setError('')
    try { await task() } catch (e) { setError(e instanceof Error ? e.message : 'Request failed') }
    finally { setBusy(false) }
  }
  function itemRow(kind: 'groups' | 'tags', item: Named) {
    const key = `${kind}/${item.id}`
    return <div className="item manage-item" key={item.id}>
      {editing === key ? <form onSubmit={e => { e.preventDefault(); action(async () => { await mutate(`/api/${key}`, 'PUT', { name: editValue }); setEditing(null); await refresh(true) }) }}>
        <input aria-label={`Rename ${item.name}`} value={editValue} onChange={e => setEditValue(e.target.value)} required />
        <button className="button">Save</button><button className="text-button" type="button" onClick={() => setEditing(null)}>Cancel</button>
      </form> : <><span>{item.name}</span>{user?.role === 'admin' && <div>
        <button className="text-button" onClick={() => { setEditing(key); setEditValue(item.name) }}>Rename</button>
        <button className="text-button destructive" onClick={() => {
          if (window.confirm(`Delete ${item.name}?`)) action(async () => { await mutate(`/api/${key}`, 'DELETE'); await refresh(true) })
        }}>Delete</button></div>}</>}
    </div>
  }
  if (authPending) return <main className="center">Loading Jump…</main>
  if (!user) return <main className="login"><div className="login-card"><div className="logo large">J<span>↗</span></div><h1>Your servers, one place.</h1><p>Secure access begins with your identity provider.</p><a className="button primary" href="/auth/login">Sign in with SSO</a></div></main>

  return <div className="layout">
    <aside className="sidebar">
      <div className="brand"><span className="logo">J<span>↗</span></span><span>Jump</span></div>
      <div className="nav-label">WORKSPACE</div>
      <nav>{nav.map(({ page: item, icon }) => <button key={item} className={page === item && !activeSessionId ? 'active' : ''} onClick={() => { setPage(item); setActiveSessionId(null); setSelected(null) }}>
        <span className="nav-icon">{icon}</span>{item}
      </button>)}</nav>
      <div className="sidebar-bottom"><div className="avatar">{user.display_name[0]?.toUpperCase()}</div><div><strong>{user.display_name}</strong><small>{user.role}</small></div></div>
    </aside>
    <main className="main">
      <header className="topbar"><div className="breadcrumbs">Workspace <span>/</span> <strong>{activeSession ? activeSession.name + ' · ' + activeSession.protocol : page}</strong></div><div className="top-right"><span className="live-dot" /> System ready</div></header>
      <Suspense fallback={null}><SessionWorkspace sessions={sessions} activeId={activeSessionId} select={id => { setActiveSessionId(id); setSelected(null) }} close={closeSession}
        fileDevices={devices.filter(d => d.online && d.capabilities.includes('file_transfer_v1')).map(d => d.id)}
        openFiles={id => { setSelected(id); setTab('Files') }} /></Suspense>
      <div className="content" hidden={Boolean(activeSessionId)}>
        {error && <div className="error" role="alert">{error}<button onClick={() => setError('')}>×</button></div>}
        <Notice notice={notice} dismiss={dismissNotice} />
        {page === 'Dashboard' && <>
          <Dashboard devices={devices} sessions={sessions} events={events} open={id => { setActiveSessionId(id); setSelected(null) }} />
        </>}
        {page === 'Devices' && <>
          <div className="heading"><div><p className="eyebrow">YOUR INFRASTRUCTURE</p><h1>Devices <span className="count">{devices.length}</span></h1><p>Enrolled endpoints and their current connection state.</p></div>
            {user.role === 'admin' && <button className="button primary" onClick={() => { setEnrolling(true); setIssued(null) }}>＋ Enroll device</button>}</div>
          <div className="filters"><input aria-label="Search devices" placeholder="⌕  Search devices…" value={query} onChange={e => setQuery(e.target.value)} />
            <select aria-label="Status" value={status} onChange={e => setStatus(e.target.value)}><option value="all">All statuses</option><option value="online">Online</option><option value="offline">Offline</option></select>
            <select aria-label="Operating system" value={os} onChange={e => setOs(e.target.value)}><option value="all">All systems</option><option value="linux">Linux</option><option value="windows">Windows</option></select>
            <select aria-label="Group" value={group} onChange={e => setGroup(e.target.value)}><option value="all">All groups</option>{groups.map(g => <option value={g.id} key={g.id}>{g.name}</option>)}</select>
            <select aria-label="Tag" value={tag} onChange={e => setTag(e.target.value)}><option value="all">All tags</option>{tags.map(t => <option value={t.id} key={t.id}>{t.name}</option>)}</select>
          </div>
          <section className="panel table-panel"><div className="table-heading"><span className="device-col-device">DEVICE</span><span className="device-col-status">STATUS</span><span className="device-col-user">CURRENT USER</span><span className="device-col-group">GROUP / TAGS</span><span className="device-col-last-seen">LAST SEEN</span><span className="device-col-connect">CONNECT</span></div>
            {visible.map(d => <div className="device-row" key={d.id} onClick={event => { if (!(event.target as HTMLElement).closest('.quick-cell')) { setSelected(d.id); setTab('Overview') } }}>
              <button className="device-row-details" aria-label={`Details for ${d.display_name || d.hostname}`} onClick={() => { setSelected(d.id); setTab('Overview') }} />
              <span className="device-name device-col-device"><span className="os-icon">{d.os_family === 'windows' ? '⊞' : '⌘'}</span><span><strong>{d.display_name || d.hostname}</strong><small>{d.hostname} · {d.os_family} {d.os_version}</small></span></span>
              <span className={`device-col-status ${d.online ? 'badge online' : 'badge offline'}`}><i />{d.identity_state === 'revoked' ? 'Revoked' : d.online ? 'Online' : 'Offline'}</span><span className="muted device-col-user">{d.current_user || '—'}</span>
              <span className="muted device-col-group">{d.group?.name || 'Ungrouped'} {d.tags.slice(0, 2).map(t => <em key={t.id}>{t.name}</em>)}</span><span className="muted device-col-last-seen">{formatDate(d.last_seen_at)}</span>
              <span className="quick-cell device-col-connect">{user.role === 'admin' && <QuickConnect device={d} preferences={quickPreferences.filter(p => p.device_id === d.id)}
                mutate={mutate} onPreference={preference => setQuickPreferences(previous => [...previous.filter(p => p.device_id !== preference.device_id || p.protocol !== preference.protocol).map(p => p.device_id === preference.device_id ? { ...p, preferred: false } : p), preference])}
                onConnected={session => { openSession(session, { activate: false }); setNotice({ text: `${session.protocol} session started for ${session.name}`, severity: 'success' }) }}
                onError={setError} />}</span>
            </div>)}
            {!visible.length && <div className="empty">{devices.length ? 'No devices match these filters.' : 'No devices yet. Enroll your first Windows or Linux server.'}</div>}
          </section>
        </>}
        {page === 'Credentials' && <Suspense fallback={null}><CredentialsPage admin={user.role === 'admin'} mutate={mutate} /></Suspense>}
        {page === 'Audit Log' && <><div className="heading"><div><p className="eyebrow">SECURITY</p><h1>Audit Log</h1><p>Recent activity in your Jump deployment.</p></div></div><section className="panel">{user.role === 'admin' ? events.map(e => <div className="activity" key={e.id}><span className="activity-mark" /><span>{e.event_type.replaceAll('_', ' ')}</span><time>{formatDate(e.created_at)}</time></div>) : <div className="empty">Admin access required.</div>}</section></>}
        {page === 'Settings' && <><div className="heading"><div><p className="eyebrow">ORGANIZE</p><h1>Settings</h1><p>Groups and tags for a single environment.</p></div></div>
          <div className="settings-grid"><section className="panel settings-panel"><h2>Groups</h2><p>Each device can belong to one group.</p>{groups.map(g => itemRow('groups', g))}
            {user.role === 'admin' && <form onSubmit={e => { e.preventDefault(); action(async () => { await mutate('/api/groups', 'POST', { name: newGroup }); setNewGroup(''); await refresh(true) }) }}><input aria-label="New group" placeholder="Group name" value={newGroup} onChange={e => setNewGroup(e.target.value)} required /><button className="button">Add</button></form>}</section>
            <section className="panel settings-panel"><h2>Tags</h2><p>Use tags to filter across groups.</p>{tags.map(t => itemRow('tags', t))}
              {user.role === 'admin' && <form onSubmit={e => { e.preventDefault(); action(async () => { await mutate('/api/tags', 'POST', { name: newTag }); setNewTag(''); await refresh(true) }) }}><input aria-label="New tag" placeholder="Tag name" value={newTag} onChange={e => setNewTag(e.target.value)} required /><button className="button">Add</button></form>}</section></div>
          <section className="panel settings-panel system-panel" aria-label="System"><h2>System</h2>
            <div className="detail"><span>Jump version</span><strong>{systemInfoError ? 'Unavailable' : systemInfo?.server_version ?? 'Loading…'}</strong></div>
            <div className="detail"><span>Target agent version</span><strong>{systemInfoError ? 'Unavailable' : systemInfo ? systemInfo.target_agent_version || 'Not configured' : 'Loading…'}</strong></div>
          </section>
          <button className="button signout" onClick={() => action(async () => { await mutate('/api/logout', 'POST'); location.reload() })}>Sign out</button>
        </>}
        {page === 'Support Links' && <><div className="heading"><div><p className="eyebrow">COMING LATER</p><h1>{page}</h1></div></div><div className="placeholder"><span>◇</span><h2>{page} is coming in a later phase</h2><p>Open a Linux device and select Terminal to start SSH.</p></div></>}
      </div>
    </main>
    {device && <div className="overlay" onClick={() => setSelected(null)}><aside className="drawer" onClick={e => e.stopPropagation()}>
      <div className="drawer-header"><span>DEVICE DETAILS</span><button aria-label="Close" onClick={() => setSelected(null)}>×</button></div>
      <div className="drawer-title"><span className="os-icon">{device.os_family === 'windows' ? '⊞' : '⌘'}</span><div><h2>{device.display_name || device.hostname}</h2><span className={device.online ? 'badge online' : 'badge offline'}><i />{device.identity_state === 'revoked' ? 'Revoked' : device.online ? 'Online' : 'Offline'}</span></div></div>
      <div className="tabs">{['Overview','Remote','Terminal','Files','Actions'].map(t => <button key={t} className={tab === t ? 'active' : ''} onClick={() => setTab(t)}>{t}</button>)}</div>
      {tab === 'Overview' ? <div className="details">
        {[['Hostname',device.hostname],['Operating system',`${device.os_family} ${device.os_version}`],['Architecture',device.architecture],['IP address',device.primary_ip || '—'],['Current user',device.current_user || '—'],['Last check-in',formatDate(device.last_seen_at)],['Agent version',device.agent_version],['Enrolled',formatDate(device.enrolled_at)],['Capabilities',device.capabilities.join(', ') || '—']].map(([key,value]) => <div className="detail" key={key}><span>{key}</span><strong>{value}</strong></div>)}
        <AgentUpdatePanel info={device.agent_update} name={device.display_name || device.hostname} online={device.online} active={device.identity_state === 'active'} admin={user.role === 'admin'} busy={busy} update={() => action(async () => { await mutate(`/api/devices/${device.id}/agent-update`, 'POST'); await refresh(true) })} />
        <div className="detail"><span>Group</span>{user.role === 'admin' ? <select value={device.group?.id || ''} onChange={e => action(async () => { await mutate(`/api/devices/${device.id}`, 'PATCH', { display_name: device.display_name, group_id: e.target.value || null, tag_ids: device.tags.map(t => t.id) }); await refresh(true) })}><option value="">Ungrouped</option>{groups.map(g => <option key={g.id} value={g.id}>{g.name}</option>)}</select> : <strong>{device.group?.name || 'Ungrouped'}</strong>}</div>
        <div className="detail"><span>Tags</span><div>{tags.map(t => <label className="tag-choice" key={t.id}><input type="checkbox" disabled={user.role !== 'admin' || busy} checked={device.tags.some(dt => dt.id === t.id)} onChange={() => action(async () => { const ids = device.tags.some(dt => dt.id === t.id) ? device.tags.filter(dt => dt.id !== t.id).map(dt => dt.id) : [...device.tags.map(dt => dt.id), t.id]; await mutate(`/api/devices/${device.id}`, 'PATCH', { display_name: device.display_name, group_id: device.group?.id || null, tag_ids: ids }); await refresh(true) })} />{t.name}</label>)}{!tags.length && '—'}</div></div>
        <section className="identity-control"><h3>Agent identity</h3><p>{device.identity_state === 'revoked' ? 'Revoked. This agent cannot reconnect. You may now permanently delete the device record.' : 'Revoking disconnects the agent and permanently rejects its current key. The device record is retained.'}</p>
          {user.role === 'admin' && device.identity_state === 'active' && <button className="button revoke-button" disabled={busy} onClick={() => {
            if (window.confirm(`Revoke ${device.display_name || device.hostname}? Its current agent will disconnect and cannot reconnect with this identity. The device record will remain.`)) {
              action(async () => { await mutate(`/api/devices/${device.id}/revoke`, 'POST'); await refresh(true) })
            }
          }}>Revoke agent identity</button>}
          {user.role === 'admin' && canDeleteDevice(device) && <button className="button revoke-button" disabled={busy || device.online} onClick={() => {
            const expected = deletionName(device)
            const typed = window.prompt(`Permanently delete ${expected}? This cannot be undone. Type "${expected}" to confirm.`)
            if (deletionConfirmed(device, typed)) {
              action(async () => {
                await mutate(`/api/devices/${device.id}`, deviceDeleteMethod)
                setSelected(null)
                await refresh(true)
              })
            } else if (typed !== null) {
              setError('Device name did not match. Nothing was deleted.')
            }
          }}>Delete device</button>}
        </section>
      </div> : tab === 'Remote' ? <Suspense fallback={<div className="placeholder compact">Loading remote desktop…</div>}><RemotePanel key={device.id} device={device} admin={user.role === 'admin'} mutate={mutate}
        existing={sessions.find((session): session is RdpSession => session.deviceId === device.id && session.protocol === 'RDP')} onConnected={openSession}
        onOpenExisting={() => { const session = sessions.find(item => item.deviceId === device.id && item.protocol === 'RDP'); if (session) { setActiveSessionId(session.id); setSelected(null) } }} /></Suspense>
      : tab === 'Terminal' ? <Suspense fallback={<div className="placeholder compact">Loading terminal…</div>}><TerminalPanel key={device.id} device={device} admin={user.role === 'admin'} mutate={mutate}
        existing={sessions.find((session): session is SshSession => session.deviceId === device.id && session.protocol === 'SSH')} onConnected={openSession}
        onOpenExisting={() => { const session = sessions.find(item => item.deviceId === device.id); if (session) { setActiveSessionId(session.id); setSelected(null) } }} /></Suspense>
      : tab === 'Files' ? user.role === 'admin' ? <Suspense fallback={<div className="placeholder compact">Loading files…</div>}><FilesPanel key={device.id} device={device} csrf={user.csrf} /></Suspense> : <div className="placeholder compact">Admin access required.</div>
      : <div className="placeholder compact"><span>◇</span><h2>{tab} is not available yet</h2><p>Connection and action features arrive in a later phase.</p></div>}
    </aside></div>}
    {enrolling && <div className="overlay" onClick={() => setEnrolling(false)}><div className="modal" onClick={e => e.stopPropagation()}><button className="close" aria-label="Close" onClick={() => setEnrolling(false)}>×</button><p className="eyebrow">NEW ENDPOINT</p><h2>Enroll a device</h2>
      {!issued && <><p>Choose the operating system, download its native agent, then generate a single-use token.</p><div className="platform">{(['linux','windows'] as Platform[]).map(v => <button key={v} type="button" aria-pressed={platform === v} className={platform === v ? 'chosen' : ''} onClick={() => setPlatform(v)}>{v === 'windows' ? '⊞  Windows amd64' : '⌘  Linux amd64'}</button>)}</div></>}
      <section className="agent-download"><div><strong>{platform === 'windows' ? 'Windows' : 'Linux'} agent</strong><span>{agentRelease?.version ? `Version ${agentRelease.version} · ${agentFilename[platform]}` : 'No release selected'}</span></div>
        {agentRelease && downloadFor(agentRelease, platform) ? <a className="button" href={downloadFor(agentRelease, platform)} target="_blank" rel="noopener noreferrer">Download {platform === 'windows' ? 'Windows' : 'Linux'} agent ↗</a> : <p>{agentRelease ? 'Set JUMP_AGENT_VERSION to a published tag to enable downloads.' : 'Loading downloads…'}</p>}
        {agentRelease?.checksums && <a className="subtle-link" href={agentRelease.checksums} target="_blank" rel="noopener noreferrer">SHA-256 checksums ↗</a>}
        {platform === 'windows' && <small>Windows builds are currently unsigned; SmartScreen may show a warning.</small>}
      </section>
      {!issued ? <button className="button primary wide" disabled={busy} onClick={() => action(async () => setIssued(await mutate('/api/enrollment-tokens', 'POST', { os_family: platform })))}>Generate token</button>
      : <><p className="warning">Shown once · expires {formatDate(issued.expires_at)}. Keep it private.</p><div className="token">{issued.token}</div><p>Run in {platform === 'windows' ? 'an elevated PowerShell window' : 'a terminal'} from the download directory:</p><pre>{enrollmentCommand(platform, issued.agent_url, issued.token)}</pre><p>The service keeps the device online after this terminal closes and starts automatically after reboot. Use <code>run</code> only for foreground troubleshooting.</p></>}
    </div></div>}
  </div>
}
