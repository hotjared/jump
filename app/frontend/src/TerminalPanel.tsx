import { useEffect, useRef, useState } from 'react'
import { SshSession } from './ssh-session'

type Credential = { id: string; label: string; username: string; kind: string }
type Device = { id: string; hostname: string; display_name: string | null; online: boolean; os_family: string; capabilities: string[]; ssh_host_key: string | null }

export default function TerminalPanel({ device, admin, mutate, existing, onConnected, onOpenExisting }: {
  device: Device; admin: boolean; mutate: <T>(url: string, method: string, body?: unknown) => Promise<T>;
  existing?: SshSession; onConnected?: (session: SshSession) => void; onOpenExisting?: () => void
}) {
  const [credentials, setCredentials] = useState<Credential[]>([])
  const [selected, setSelected] = useState('')
  const [status, setStatus] = useState('Disconnected')
  const [fingerprint, setFingerprint] = useState(device.ssh_host_key)
  const [error, setError] = useState('')
  const pending = useRef<SshSession | null>(null)
  const mounted = useRef(true)

  useEffect(() => {
    mounted.current = true
    if (admin) fetch(`/api/credentials`, { credentials: 'same-origin' })
      .then(r => { if (!r.ok) throw new Error('Could not load credentials'); return r.json() as Promise<Credential[]> })
      .then(items => { if (mounted.current) setCredentials(items.filter(item => ['linux_password', 'linux_ssh_key'].includes(item.kind))) })
      .catch(e => { if (mounted.current) setError(e.message) })
    return () => { mounted.current = false; pending.current?.disconnect(); pending.current = null }
  }, [admin, device.id])

  async function connect() {
    if (!selected || pending.current) return
    setError(''); setStatus('Connecting')
    try {
      const response = await mutate<{ id: string }>(`/api/devices/${device.id}/ssh-sessions`, 'POST',
        { credential_id: selected, columns: 80, rows: 24 })
      const session = new SshSession(response.id, device.id, device.display_name || device.hostname, device.os_family)
      if (!mounted.current) { session.disconnect(); return }
      pending.current = session
      await session.ready()
      if (!mounted.current) { session.disconnect(); return }
      pending.current = null
      onConnected?.(session)
    } catch (e) {
      pending.current?.disconnect(); pending.current = null
      if (mounted.current) { setStatus('Disconnected'); setError(e instanceof Error ? e.message : 'Could not start SSH session') }
    }
  }


  if (device.os_family !== 'linux')
    return <div className="placeholder compact"><h2>Terminal unavailable</h2><p>This agent does not advertise Linux SSH support.</p></div>
  if (!device.capabilities.includes('ssh_terminal_v1'))
    return <div className="placeholder compact"><h2>Terminal unavailable</h2><p>Update the Jump agent to enable browser SSH.</p></div>
  if (!admin)
    return <div className="placeholder compact"><h2>Admin access required</h2><p>SSH access is currently limited to Jump administrators.</p></div>
  if (existing && (existing.state === 'connected' || existing.state === 'connecting'))
    return <div className="terminal-panel"><h3>SSH session active</h3><p className="muted">{device.display_name || device.hostname} is already open in the workspace.</p>
      <button className="button primary" onClick={onOpenExisting}>Open session</button></div>

  return <div className="terminal-panel">
    <p className="muted">SSH through the connected Jump agent to localhost:22.</p>
    {!device.online && <p role="status">Offline. Connect the agent before starting a terminal.</p>}
    {fingerprint && <p className="fingerprint">Trusted SSH host key: <code>{fingerprint}</code></p>}
    {fingerprint && <button className="text-button" onClick={async () => {
      if (!window.confirm('Reset the trusted SSH host key? Verify the new fingerprint before connecting again.')) return
      try { await mutate(`/api/devices/${device.id}/ssh-host-key/reset`, 'POST'); setFingerprint(null) }
      catch (e) { setError(e instanceof Error ? e.message : 'Could not reset key') }
    }}>Reset trusted key</button>}
    <div className="terminal-controls">
      <select aria-label="SSH credential" value={selected} onChange={e => setSelected(e.target.value)} disabled={status === 'Connecting'}>
        <option value="">Choose SSH credential</option>{credentials.map(c => <option key={c.id} value={c.id}>{c.label} ({c.username})</option>)}
      </select>
      <button className="button primary" disabled={!device.online || !selected || status === 'Connecting'} onClick={connect}>Connect</button>
    </div>
    {!credentials.length && <p>No SSH credentials saved. Add one under Credentials.</p>}
    {error && <p className="error" role="alert">{error}</p>}
    <p role="status">{status}</p>

  </div>
}
