import { useEffect, useMemo, useState } from 'react'
import { agentFilename, downloadFor, enrollmentCommand, type AgentDownloads, type Platform } from './agent-downloads'

type Named = { id: string; name: string }
type Device = {
  id: string; device_uuid: string; hostname: string; display_name: string | null;
  os_family: string; os_version: string; architecture: string; agent_version: string;
  capabilities: string[]; addresses: string[]; primary_ip: string | null;
  current_user: string | null; group: Named | null; tags: Named[];
  online: boolean; identity_state: "active" | "revoked" | "none"; last_seen_at: string | null; enrolled_at: string;
}
type User = { id: string; email: string; display_name: string; role: string; csrf: string }
type Event = { id: string; event_type: string; created_at: string; device_id: string | null }
type Page = 'Dashboard' | 'Devices' | 'Sessions' | 'Support Links' | 'Audit Log' | 'Settings'
const nav: Page[] = ['Dashboard', 'Devices', 'Sessions', 'Support Links', 'Audit Log', 'Settings']
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
  const [devices, setDevices] = useState<Device[]>([])
  const [groups, setGroups] = useState<Named[]>([])
  const [tags, setTags] = useState<Named[]>([])
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
  const visible = useMemo(() => filterDevices(devices, query, status, os, group, tag),
    [devices, query, status, os, group, tag])
  const device = devices.find(d => d.id === selected)
  const online = devices.filter(d => d.online).length

  async function mutate<T>(url: string, method: string, body?: unknown): Promise<T> {
    const response = await fetch(url, {
      method, credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': user!.csrf },
      body: body ? JSON.stringify(body) : undefined,
    })
    if (!response.ok) throw new Error(`Request failed: ${response.status}`)
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
      <nav>{nav.map((item, i) => <button key={item} className={page === item ? 'active' : ''} onClick={() => { setPage(item); setSelected(null) }}>
        <span className="nav-icon">{['◫', '▤', '▣', '↗', '≡', '⚙'][i]}</span>{item}
      </button>)}</nav>
      <div className="sidebar-bottom"><div className="avatar">{user.display_name[0]?.toUpperCase()}</div><div><strong>{user.display_name}</strong><small>{user.role}</small></div></div>
    </aside>
    <main className="main">
      <header className="topbar"><div className="breadcrumbs">Workspace <span>/</span> <strong>{page}</strong></div><div className="top-right"><span className="live-dot" /> System ready</div></header>
      <div className="content">
        {error && <div className="error" role="alert">{error}<button onClick={() => setError('')}>×</button></div>}
        {page === 'Dashboard' && <>
          <div className="heading"><div><p className="eyebrow">OVERVIEW</p><h1>Dashboard</h1><p>Device presence and recent activity at a glance.</p></div></div>
          <div className="stats"><div className="stat"><small>TOTAL DEVICES</small><strong>{devices.length}</strong></div><div className="stat"><small>ONLINE</small><strong className="green">{online}</strong></div><div className="stat"><small>OFFLINE</small><strong>{devices.length - online}</strong></div></div>
          <section className="panel"><div className="panel-title">Recent activity</div>{events.length ? events.slice(0, 8).map(e => <div className="activity" key={e.id}><span className="activity-mark" /><span>{e.event_type.replaceAll('_', ' ')}</span><time>{formatDate(e.created_at)}</time></div>) : <div className="empty">No recent activity to show.</div>}</section>
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
          <section className="panel table-panel"><div className="table-heading"><span>DEVICE</span><span>STATUS</span><span>CURRENT USER</span><span>GROUP / TAGS</span><span>LAST SEEN</span></div>
            {visible.map(d => <button className="device-row" key={d.id} onClick={() => { setSelected(d.id); setTab('Overview') }}>
              <span className="device-name"><span className="os-icon">{d.os_family === 'windows' ? '⊞' : '⌘'}</span><span><strong>{d.display_name || d.hostname}</strong><small>{d.hostname} · {d.os_family} {d.os_version}</small></span></span>
              <span className={d.online ? 'badge online' : 'badge offline'}><i />{d.identity_state === 'revoked' ? 'Revoked' : d.online ? 'Online' : 'Offline'}</span><span className="muted">{d.current_user || '—'}</span>
              <span className="muted">{d.group?.name || 'Ungrouped'} {d.tags.slice(0, 2).map(t => <em key={t.id}>{t.name}</em>)}</span><span className="muted">{formatDate(d.last_seen_at)}</span>
            </button>)}
            {!visible.length && <div className="empty">{devices.length ? 'No devices match these filters.' : 'No devices yet. Enroll your first Windows or Linux server.'}</div>}
          </section>
        </>}
        {page === 'Audit Log' && <><div className="heading"><div><p className="eyebrow">SECURITY</p><h1>Audit Log</h1><p>Recent activity in your Jump deployment.</p></div></div><section className="panel">{user.role === 'admin' ? events.map(e => <div className="activity" key={e.id}><span className="activity-mark" /><span>{e.event_type.replaceAll('_', ' ')}</span><time>{formatDate(e.created_at)}</time></div>) : <div className="empty">Admin access required.</div>}</section></>}
        {page === 'Settings' && <><div className="heading"><div><p className="eyebrow">ORGANIZE</p><h1>Settings</h1><p>Groups and tags for a single environment.</p></div></div>
          <div className="settings-grid"><section className="panel settings-panel"><h2>Groups</h2><p>Each device can belong to one group.</p>{groups.map(g => itemRow('groups', g))}
            {user.role === 'admin' && <form onSubmit={e => { e.preventDefault(); action(async () => { await mutate('/api/groups', 'POST', { name: newGroup }); setNewGroup(''); await refresh(true) }) }}><input aria-label="New group" placeholder="Group name" value={newGroup} onChange={e => setNewGroup(e.target.value)} required /><button className="button">Add</button></form>}</section>
            <section className="panel settings-panel"><h2>Tags</h2><p>Use tags to filter across groups.</p>{tags.map(t => itemRow('tags', t))}
              {user.role === 'admin' && <form onSubmit={e => { e.preventDefault(); action(async () => { await mutate('/api/tags', 'POST', { name: newTag }); setNewTag(''); await refresh(true) }) }}><input aria-label="New tag" placeholder="Tag name" value={newTag} onChange={e => setNewTag(e.target.value)} required /><button className="button">Add</button></form>}</section></div>
          <button className="button signout" onClick={() => action(async () => { await mutate('/api/logout', 'POST'); location.reload() })}>Sign out</button>
        </>}
        {(page === 'Sessions' || page === 'Support Links') && <><div className="heading"><div><p className="eyebrow">COMING LATER</p><h1>{page}</h1></div></div><div className="placeholder"><span>◇</span><h2>{page} is coming in a later phase</h2><p>The foundation is ready; remote access is not enabled yet.</p></div></>}
      </div>
    </main>
    {device && <div className="overlay" onClick={() => setSelected(null)}><aside className="drawer" onClick={e => e.stopPropagation()}>
      <div className="drawer-header"><span>DEVICE DETAILS</span><button aria-label="Close" onClick={() => setSelected(null)}>×</button></div>
      <div className="drawer-title"><span className="os-icon">{device.os_family === 'windows' ? '⊞' : '⌘'}</span><div><h2>{device.display_name || device.hostname}</h2><span className={device.online ? 'badge online' : 'badge offline'}><i />{device.identity_state === 'revoked' ? 'Revoked' : device.online ? 'Online' : 'Offline'}</span></div></div>
      <div className="tabs">{['Overview','Remote','Terminal','Files','Actions'].map(t => <button key={t} className={tab === t ? 'active' : ''} onClick={() => setTab(t)}>{t}</button>)}</div>
      {tab === 'Overview' ? <div className="details">
        {[['Hostname',device.hostname],['Operating system',`${device.os_family} ${device.os_version}`],['Architecture',device.architecture],['IP address',device.primary_ip || '—'],['Current user',device.current_user || '—'],['Last check-in',formatDate(device.last_seen_at)],['Agent version',device.agent_version],['Enrolled',formatDate(device.enrolled_at)],['Capabilities',device.capabilities.join(', ') || '—']].map(([key,value]) => <div className="detail" key={key}><span>{key}</span><strong>{value}</strong></div>)}
        <div className="detail"><span>Group</span>{user.role === 'admin' ? <select value={device.group?.id || ''} onChange={e => action(async () => { await mutate(`/api/devices/${device.id}`, 'PATCH', { display_name: device.display_name, group_id: e.target.value || null, tag_ids: device.tags.map(t => t.id) }); await refresh(true) })}><option value="">Ungrouped</option>{groups.map(g => <option key={g.id} value={g.id}>{g.name}</option>)}</select> : <strong>{device.group?.name || 'Ungrouped'}</strong>}</div>
        <div className="detail"><span>Tags</span><div>{tags.map(t => <label className="tag-choice" key={t.id}><input type="checkbox" disabled={user.role !== 'admin' || busy} checked={device.tags.some(dt => dt.id === t.id)} onChange={() => action(async () => { const ids = device.tags.some(dt => dt.id === t.id) ? device.tags.filter(dt => dt.id !== t.id).map(dt => dt.id) : [...device.tags.map(dt => dt.id), t.id]; await mutate(`/api/devices/${device.id}`, 'PATCH', { display_name: device.display_name, group_id: device.group?.id || null, tag_ids: ids }); await refresh(true) })} />{t.name}</label>)}{!tags.length && '—'}</div></div>
        <section className="identity-control"><h3>Agent identity</h3><p>{device.identity_state === 'revoked' ? 'Revoked. This agent cannot reconnect. The device record and audit history are retained.' : 'Revoking disconnects the agent and permanently rejects its current key. The device record is retained.'}</p>
          {user.role === 'admin' && device.identity_state === 'active' && <button className="button revoke-button" disabled={busy} onClick={() => {
            if (window.confirm(`Revoke ${device.display_name || device.hostname}? Its current agent will disconnect and cannot reconnect with this identity. The device record will remain.`)) {
              action(async () => { await mutate(`/api/devices/${device.id}/revoke`, 'POST'); await refresh(true) })
            }
          }}>Revoke agent identity</button>}
        </section>
      </div> : <div className="placeholder compact"><span>◇</span><h2>{tab} is not available yet</h2><p>Connection and action features arrive in a later phase.</p></div>}
    </aside></div>}
    {enrolling && <div className="overlay" onClick={() => setEnrolling(false)}><div className="modal" onClick={e => e.stopPropagation()}><button className="close" aria-label="Close" onClick={() => setEnrolling(false)}>×</button><p className="eyebrow">NEW ENDPOINT</p><h2>Enroll a device</h2>
      {!issued && <><p>Choose the operating system, download its native agent, then generate a single-use token.</p><div className="platform">{(['linux','windows'] as Platform[]).map(v => <button key={v} type="button" aria-pressed={platform === v} className={platform === v ? 'chosen' : ''} onClick={() => setPlatform(v)}>{v === 'windows' ? '⊞  Windows amd64' : '⌘  Linux amd64'}</button>)}</div></>}
      <section className="agent-download"><div><strong>{platform === 'windows' ? 'Windows' : 'Linux'} agent</strong><span>{agentRelease?.version ? `Version ${agentRelease.version} · ${agentFilename[platform]}` : 'No release selected'}</span></div>
        {agentRelease && downloadFor(agentRelease, platform) ? <a className="button" href={downloadFor(agentRelease, platform)} target="_blank" rel="noopener noreferrer">Download {platform === 'windows' ? 'Windows' : 'Linux'} agent ↗</a> : <p>{agentRelease ? 'Set JUMP_AGENT_VERSION to a published tag to enable downloads.' : 'Loading downloads…'}</p>}
        {agentRelease?.checksums && <a className="subtle-link" href={agentRelease.checksums} target="_blank" rel="noopener noreferrer">SHA-256 checksums ↗</a>}
        {platform === 'windows' && <small>Windows builds are currently unsigned; SmartScreen may show a warning.</small>}
      </section>
      {!issued ? <button className="button primary wide" disabled={busy} onClick={() => action(async () => setIssued(await mutate('/api/enrollment-tokens', 'POST', { os_family: platform })))}>Generate token</button>
      : <><p className="warning">Shown once · expires {formatDate(issued.expires_at)}. Keep it private.</p><div className="token">{issued.token}</div><p>Run in {platform === 'windows' ? 'an elevated PowerShell window' : 'a terminal'} from the download directory:</p><pre>{enrollmentCommand(platform, issued.agent_url, issued.token)}</pre><p>For persistent presence, install the run command as a service.</p></>}
    </div></div>}
  </div>
}
