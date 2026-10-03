import { useState } from 'react'
import { ScreenSession } from './screen-session'

type Device = { id: string; hostname: string; display_name: string | null; online: boolean; os_family: string; capabilities: string[] }
export default function ScreenPanel({ device, admin, mutate, onConnected, existing, openExisting }: {
  device: Device; admin: boolean; mutate: <T>(url: string, method: string, body?: unknown) => Promise<T>;
  onConnected: (session: ScreenSession) => void; existing?: ScreenSession; openExisting: () => void
}) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [recoverId, setRecoverId] = useState<string | null>(null)
  async function connect(replaceId?: string) {
    setBusy(true); setError('')
    try {
      if (replaceId) await mutate(`/api/devices/${device.id}/screen-sessions/${replaceId}/close`, 'POST', {})
      setRecoverId(null)
      const result = await mutate<{ id: string }>(`/api/devices/${device.id}/screen-sessions`, 'POST', {})
      onConnected(new ScreenSession(result.id, device.id, device.display_name || device.hostname, device.os_family, device.capabilities))
    } catch (e) {
      const message = e instanceof Error ? e.message : 'Could not start Screen Control'
      setError(message)
      try {
        const response = await fetch(`/api/devices/${device.id}/screen-sessions/current`, { credentials: 'same-origin' })
        if (response.ok) {
          const current = await response.json() as { id: string | null }
          setRecoverId(current.id)
          if (!current.id && message === 'A Screen Control session is already active.')
            setError('Screen Control is in use. Try again after the current session ends.')
        }
      } catch { /* Keep the original error and allow another attempt. */ }
    }
    finally { setBusy(false) }
  }
  return <section className="terminal-panel remote-section"><h3>Screen Control</h3>
    <p className="muted">View and control the desktop currently visible on the device. No Windows credentials or RDP required.</p>
    {!admin ? <p>Admin access required.</p> : !device.capabilities.some(capability => ['screen_control_v1', 'screen_control_v2'].includes(capability)) ? <p>Update the Windows Jump agent service to enable Screen Control.</p>
      : existing && ['connected', 'connecting'].includes(existing.state) ? <button className="button" onClick={openExisting}>Open Screen session</button>
      : <button className="button primary" disabled={busy || !device.online} onClick={() => void connect()}>Screen Control</button>}
    {error && <p role="alert" className="error">{error}</p>}
    {admin && recoverId && !(existing && ['connected', 'connecting'].includes(existing.state)) && <>
      <p className="muted">This ends your existing Screen session, including any browser still controlling it.</p>
      <button className="button" disabled={busy} onClick={() => void connect(recoverId)}>End existing session and reconnect</button>
    </>}
  </section>
}
