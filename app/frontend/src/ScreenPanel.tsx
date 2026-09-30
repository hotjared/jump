import { useState } from 'react'
import { ScreenSession } from './screen-session'

type Device = { id: string; hostname: string; display_name: string | null; online: boolean; os_family: string; capabilities: string[] }
export default function ScreenPanel({ device, admin, mutate, onConnected, existing, openExisting }: {
  device: Device; admin: boolean; mutate: <T>(url: string, method: string, body?: unknown) => Promise<T>;
  onConnected: (session: ScreenSession) => void; existing?: ScreenSession; openExisting: () => void
}) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  async function connect() {
    setBusy(true); setError('')
    try {
      const result = await mutate<{ id: string }>(`/api/devices/${device.id}/screen-sessions`, 'POST', {})
      onConnected(new ScreenSession(result.id, device.id, device.display_name || device.hostname, device.os_family))
    } catch (e) { setError(e instanceof Error ? e.message : 'Could not start Screen Control') }
    finally { setBusy(false) }
  }
  return <section className="terminal-panel"><h3>Screen Control</h3>
    <p className="muted">View and control the desktop currently visible on the device. No Windows credentials or RDP required.</p>
    {!admin ? <p>Admin access required.</p> : !device.capabilities.includes('screen_control_v1') ? <p>Update the Windows Jump agent service to enable Screen Control.</p>
      : existing && ['connected', 'connecting'].includes(existing.state) ? <button className="button" onClick={openExisting}>Open Screen session</button>
      : <button className="button primary" disabled={busy || !device.online} onClick={() => void connect()}>Screen Control</button>}
    {error && <p role="alert" className="error">{error}</p>}
  </section>
}
