import { useRef, useState } from 'react'
import { RdpSession } from './rdp-session'
import { SshSession } from './ssh-session'
import type { WorkspaceSession } from './SessionWorkspace'

export type Protocol = 'rdp' | 'ssh'
export type QuickPreference = { device_id: string; protocol: Protocol; credential_id: string; preferred: boolean }
type Credential = { id: string; label: string; username: string; kind: string }
type Device = { id: string; hostname: string; display_name: string | null; os_family: string; online: boolean; capabilities: string[]; identity_state: string }

export function supportedProtocols(device: Device): Protocol[] {
  if (!device.online || device.identity_state !== 'active') return []
  return [
    ...(device.os_family === 'windows' && device.capabilities.includes('rdp_tunnel_v1') ? ['rdp' as const] : []),
    ...(device.os_family === 'linux' && device.capabilities.includes('ssh_terminal_v1') ? ['ssh' as const] : []),
  ]
}

function compatible(credential: Credential, protocol: Protocol) {
  return protocol === 'rdp' ? credential.kind === 'windows_password' : ['linux_password', 'linux_ssh_key'].includes(credential.kind)
}

export default function QuickConnect({ device, preferences, mutate, onPreference, onConnected, onError }: {
  device: Device; preferences: QuickPreference[];
  mutate: <T>(url: string, method: string, body?: unknown) => Promise<T>;
  onPreference: (preference: QuickPreference) => void;
  onConnected: (session: WorkspaceSession) => void;
  onError: (error: string) => void;
}) {
  const protocols = supportedProtocols(device)
  const preferred = preferences.find(p => p.preferred && protocols.includes(p.protocol))
  const defaultProtocol = preferred?.protocol || protocols[0]
  const [menu, setMenu] = useState(false)
  const [config, setConfig] = useState(false)
  const [protocol, setProtocol] = useState<Protocol>('rdp')
  const [credentials, setCredentials] = useState<Credential[]>([])
  const [credentialId, setCredentialId] = useState('')
  const [remember, setRemember] = useState(true)
  const [pending, setPending] = useState(false)
  const pendingRef = useRef(false)

  if (!protocols.length) return null
  const name = device.display_name || device.hostname

  async function loadCredentials(): Promise<Credential[]> {
    const response = await fetch(`/api/devices/${device.id}/credentials`, { credentials: 'same-origin' })
    if (!response.ok) throw new Error('Could not load credentials')
    return response.json() as Promise<Credential[]>
  }

  async function configure(selected: Protocol, available: Credential[], chosen = '') {
    setProtocol(selected)
    setCredentials(available)
    setCredentialId(chosen)
    setRemember(true)
    setConfig(true)
  }

  async function connect(selected: Protocol, credential: string) {
    const url = `/api/devices/${device.id}/${selected === 'rdp' ? 'rdp' : 'ssh'}-sessions`
    const body = selected === 'rdp'
      ? { credential_id: credential, width: Math.max(640, window.innerWidth - 320), height: Math.max(480, window.innerHeight - 180), dpi: 96 }
      : { credential_id: credential, columns: 80, rows: 24 }
    const result = await mutate<{ id: string }>(url, 'POST', body)
    const session = selected === 'rdp'
      ? new RdpSession(result.id, device.id, name, device.os_family)
      : new SshSession(result.id, device.id, name, device.os_family)
    try {
      if (session instanceof SshSession) await session.ready()
      onConnected(session)
      setConfig(false)
    } catch {
      session.disconnect()
      throw new Error('Could not start session. Check the device and credential, then retry.')
    }
  }

  async function start(selected: Protocol, fromSettings = false) {
    if (pendingRef.current) return
    pendingRef.current = true; setPending(true); setMenu(false); onError('')
    try {
      const available = (await loadCredentials()).filter(c => compatible(c, selected))
      const saved = preferences.find(p => p.protocol === selected)
      const matching = available.find(c => c.id === saved?.credential_id)
      if (fromSettings || !matching) {
        await configure(selected, available, matching?.id || '')
        if (saved && !matching) onError('Saved credential is unavailable. Choose another credential.')
      } else await connect(selected, matching.id)
    } catch { onError('Could not start session. Check the device and credential, then retry.') }
    finally { pendingRef.current = false; setPending(false) }
  }

  async function submit(event: React.FormEvent) {
    event.preventDefault()
    if (!credentialId || pendingRef.current) return
    pendingRef.current = true; setPending(true); onError('')
    try {
      if (remember) {
        const preference = await mutate<QuickPreference>(`/api/devices/${device.id}/quick-connect-preferences`, 'PUT',
          { protocol, credential_id: credentialId, preferred: true })
        onPreference(preference)
      }
      await connect(protocol, credentialId)
    } catch { onError('Could not start session. Check the device and credential, then retry.') }
    finally { pendingRef.current = false; setPending(false) }
  }

  return <div className="quick-connect">
    <button className="button primary" disabled={pending} onClick={() => void start(defaultProtocol)}>{pending ? 'Connecting…' : `Connect ${defaultProtocol.toUpperCase()}`}</button>
    <button className="button quick-chevron" aria-label={`Quick Connect options for ${name}`} aria-expanded={menu} disabled={pending} onClick={() => setMenu(!menu)}>▾</button>
    {menu && <div className="quick-menu">{protocols.map(p => <button key={p} onClick={() => void start(p)}>Connect with {p.toUpperCase()}</button>)}
      <button onClick={() => void start(defaultProtocol, true)}>Quick Connect settings…</button></div>}
    {config && <div className="overlay" onClick={() => setConfig(false)}><form className="modal quick-modal" onClick={event => event.stopPropagation()} onSubmit={event => void submit(event)}>
      <button className="close" type="button" aria-label="Close Quick Connect" onClick={() => setConfig(false)}>×</button>
      <h2>Quick Connect · {name}</h2>
      {protocols.length > 1 && <label>Connection<select aria-label="Quick Connect connection" value={protocol} onChange={event => { const next = event.target.value as Protocol; setProtocol(next); setCredentialId('') }}>
        {protocols.map(p => <option value={p} key={p}>{p === 'rdp' ? 'Remote Desktop' : 'SSH'}</option>)}</select></label>}
      <label>Credential<select aria-label="Quick Connect credential" value={credentialId} onChange={event => setCredentialId(event.target.value)}>
        <option value="">Choose credential</option>{credentials.filter(c => compatible(c, protocol)).map(c => <option key={c.id} value={c.id}>{c.label} ({c.username})</option>)}
      </select></label>
      {!credentials.some(c => compatible(c, protocol)) && <p>No saved credential for this connection. Add one in Device Details.</p>}
      <label className="quick-remember"><input type="checkbox" checked={remember} onChange={event => setRemember(event.target.checked)} /> Use this for Quick Connect</label>
      <button className="button primary" disabled={pending || !credentialId}>{pending ? 'Connecting…' : 'Connect'}</button>
    </form></div>}
  </div>
}
