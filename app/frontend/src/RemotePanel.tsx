import { useEffect, useState } from 'react'
import { RdpSession } from './rdp-session'

type Credential = { id: string; label: string; username: string; kind: string; domain?: string | null }
type Device = { id: string; hostname: string; display_name: string | null; online: boolean; os_family: string; capabilities: string[] }

export default function RemotePanel({ device, admin, mutate, existing, onConnected, onOpenExisting }: {
  device: Device; admin: boolean; mutate: <T>(url: string, method: string, body?: unknown) => Promise<T>;
  existing?: RdpSession; onConnected: (session: RdpSession) => void; onOpenExisting: () => void
}) {
  const [credentials, setCredentials] = useState<Credential[]>([])
  const [selected, setSelected] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  useEffect(() => {
    if (!admin || device.os_family !== 'windows') return
    let mounted = true
    fetch(`/api/credentials`, { credentials: 'same-origin' })
      .then(r => { if (!r.ok) throw new Error('Could not load credentials'); return r.json() as Promise<Credential[]> })
      .then(items => { if (mounted) setCredentials(items.filter(c => c.kind === 'windows_password')) })
      .catch(e => { if (mounted) setError(e.message) })
    return () => { mounted = false }
  }, [admin, device.id, device.os_family])

  if (device.os_family !== 'windows')
    return <div className="placeholder compact"><h2>Remote desktop unavailable</h2><p>Browser RDP is available for Windows devices.</p></div>
  if (!device.capabilities.includes('rdp_tunnel_v1'))
    return <div className="placeholder compact"><h2>Remote desktop unavailable</h2><p>Update the Jump agent to enable browser RDP.</p></div>
  if (!admin)
    return <div className="placeholder compact"><h2>Admin access required</h2><p>RDP access is currently limited to Jump administrators.</p></div>
  if (existing && ['connected', 'connecting'].includes(existing.state))
    return <div className="terminal-panel"><h3>RDP session active</h3><p className="muted">{device.display_name || device.hostname} is open in the workspace.</p><button className="button primary" onClick={onOpenExisting}>Open session</button></div>

  async function connect() {
    if (!selected || busy) return
    setBusy(true); setError('')
    try {
      const result = await mutate<{ id: string }>(`/api/devices/${device.id}/rdp-sessions`, 'POST', {
        credential_id: selected, width: Math.max(640, window.innerWidth - 320), height: Math.max(480, window.innerHeight - 180), dpi: 96,
      })
      onConnected(new RdpSession(result.id, device.id, device.display_name || device.hostname, device.os_family))
    } catch (e) { setError(e instanceof Error ? e.message : 'Could not start RDP session') }
    finally { setBusy(false) }
  }


  return <div className="terminal-panel">
    <p className="muted">RDP through the connected Jump agent to Windows localhost:3389. Enable Remote Desktop on the device first.</p>
    {!device.online && <p role="status">Offline. Connect the agent before starting RDP.</p>}
    <div className="terminal-controls"><select aria-label="Windows credential" value={selected} onChange={e => setSelected(e.target.value)}>
      <option value="">Choose Windows credential</option>{credentials.map(c => <option key={c.id} value={c.id}>{c.label} ({c.domain ? `${c.domain}\\` : ''}{c.username})</option>)}
    </select><button className="button primary" disabled={!device.online || !selected || busy} onClick={connect}>Connect</button></div>
    {!credentials.length && <p>No Windows credentials saved. Add one under Credentials.</p>}
    {error && <p className="error" role="alert">{error}</p>}

  </div>
}
