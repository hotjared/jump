import { useEffect, useRef, useState } from 'react'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import '@xterm/xterm/css/xterm.css'

type Credential = { id: string; label: string; username: string; kind: string }
type Device = { id: string; online: boolean; os_family: string; capabilities: string[]; ssh_host_key: string | null }
type Frame = { type: string; data?: string; state?: string; code?: string; message?: string; fingerprint?: string }

export default function TerminalPanel({ device, admin, mutate }: {
  device: Device; admin: boolean; mutate: <T>(url: string, method: string, body?: unknown) => Promise<T>
}) {
  const [credentials, setCredentials] = useState<Credential[]>([])
  const [selected, setSelected] = useState('')
  const [status, setStatus] = useState('Disconnected')
  const [fingerprint, setFingerprint] = useState(device.ssh_host_key)
  const [kind, setKind] = useState<'linux_password' | 'linux_ssh_key'>('linux_password')
  const [label, setLabel] = useState('')
  const [username, setUsername] = useState('')
  const [secret, setSecret] = useState('')
  const [error, setError] = useState('')
  const node = useRef<HTMLDivElement>(null)
  const terminal = useRef<Terminal | null>(null)
  const socket = useRef<WebSocket | null>(null)
  const resize = useRef<ResizeObserver | null>(null)
  const input = useRef<{ dispose: () => void } | null>(null)
  const connected = useRef(false)

  useEffect(() => {
    if (!admin) return
    fetch(`/api/devices/${device.id}/credentials`, { credentials: 'same-origin' })
      .then(r => { if (!r.ok) throw new Error('Could not load credentials'); return r.json() as Promise<Credential[]> })
      .then(items => { setCredentials(items.filter(item => ['linux_password', 'linux_ssh_key'].includes(item.kind))) })
      .catch(e => setError(e.message))
    return () => { socket.current?.close(); resize.current?.disconnect(); input.current?.dispose(); terminal.current?.dispose() }
  }, [admin, device.id])

  function setupTerminal(ws: WebSocket) {
    if (!node.current) return
    terminal.current?.dispose()
    const term = new Terminal({ cursorBlink: true, convertEol: true, fontSize: 14, theme: { background: '#111318' } })
    const fit = new FitAddon()
    term.loadAddon(fit)
    term.open(node.current)
    fit.fit()
    terminal.current = term
    input.current = term.onData(data => {
      if (ws.readyState !== WebSocket.OPEN || !connected.current) return
      const bytes = new TextEncoder().encode(data)
      let binary = ''
      bytes.forEach(byte => { binary += String.fromCharCode(byte) })
      ws.send(JSON.stringify({ type: 'session_data', data: btoa(binary) }))
    })
    resize.current?.disconnect()
    resize.current = new ResizeObserver(() => {
      fit.fit()
      if (ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ type: 'session_resize', columns: term.cols, rows: term.rows }))
    })
    resize.current.observe(node.current)
  }

  async function connect() {
    if (!selected || socket.current) return
    setError(''); setStatus('Connecting'); connected.current = false
    try {
      const session = await mutate<{ id: string }>(`/api/devices/${device.id}/ssh-sessions`, 'POST',
        { credential_id: selected, columns: 80, rows: 24 })
      const url = new URL(`/ws/sessions/${session.id}`, location.href)
      url.protocol = location.protocol === 'https:' ? 'wss:' : 'ws:'
      const ws = new WebSocket(url)
      socket.current = ws
      ws.onopen = () => setupTerminal(ws)
      ws.onmessage = event => {
        const frame = JSON.parse(event.data as string) as Frame
        if (frame.type === 'session_data' && frame.data) {
          const binary = atob(frame.data)
          terminal.current?.write(Uint8Array.from(binary, c => c.charCodeAt(0)))
        } else if (frame.type === 'status') {
          if (frame.state === 'active') {
            connected.current = true; setStatus('Connected'); setFingerprint(frame.fingerprint || null)
            if (terminal.current) ws.send(JSON.stringify({ type: 'session_resize', columns: terminal.current.cols, rows: terminal.current.rows }))
            terminal.current?.focus()
          }
          if (frame.state === 'closed') { setStatus('Disconnected'); if (frame.code && frame.code !== 'session_closed') setError(frame.message || 'Session disconnected') }
        }
      }
      ws.onerror = () => setError('Could not connect to the session gateway')
      ws.onclose = () => { socket.current = null; connected.current = false; setStatus('Disconnected'); resize.current?.disconnect() }
    } catch (e) { setStatus('Disconnected'); setError(e instanceof Error ? e.message : 'Could not start SSH session') }
  }

  async function saveCredential(event: React.FormEvent) {
    event.preventDefault(); setError('')
    try {
      const item = await mutate<Credential>(`/api/devices/${device.id}/credentials`, 'POST', { label, kind, username, secret })
      setCredentials(previous => [...previous, item]); setSelected(item.id)
      setLabel(''); setUsername(''); setSecret('')
    } catch (e) { setError(e instanceof Error ? e.message : 'Could not save credential') }
  }

  if (device.os_family !== 'linux')
    return <div className="placeholder compact"><h2>Terminal unavailable</h2><p>This agent does not advertise Linux SSH support.</p></div>
  if (!device.capabilities.includes('ssh_terminal_v1'))
    return <div className="placeholder compact"><h2>Terminal unavailable</h2><p>Update the Jump agent to enable browser SSH.</p></div>
  if (!admin)
    return <div className="placeholder compact"><h2>Admin access required</h2><p>SSH access is currently limited to Jump administrators.</p></div>

  return <div className="terminal-panel">
    <p className="muted">SSH through the connected Jump agent to localhost:22.</p>
    {!device.online && <p role="status">Offline. Connect the agent before starting a terminal.</p>}
    {fingerprint && <p className="fingerprint">Trusted SSH host key: <code>{fingerprint}</code></p>}
    {admin && fingerprint && <button className="text-button" onClick={async () => {
      if (!window.confirm('Reset the trusted SSH host key? Verify the new fingerprint before connecting again.')) return
      try { await mutate(`/api/devices/${device.id}/ssh-host-key/reset`, 'POST'); setFingerprint(null) }
      catch (e) { setError(e instanceof Error ? e.message : 'Could not reset key') }
    }}>Reset trusted key</button>}
    <div className="terminal-controls">
      <select aria-label="SSH credential" value={selected} onChange={e => setSelected(e.target.value)} disabled={Boolean(socket.current)}>
        <option value="">Choose SSH credential</option>{credentials.map(c => <option key={c.id} value={c.id}>{c.label} ({c.username})</option>)}
      </select>
      {socket.current ? <button className="button" onClick={() => { socket.current?.close(); socket.current = null; setStatus('Disconnected') }}>Disconnect</button>
        : <button className="button primary" disabled={!device.online || !selected || status === 'Connecting'} onClick={connect}>Connect</button>}
    </div>
    {!credentials.length && <p>No SSH credential saved for this device.</p>}
    {error && <p className="error" role="alert">{error}</p>}
    <p role="status">{status}</p>
    <div className="terminal-screen" ref={node} aria-label="SSH terminal" />
    {admin && <form className="ssh-credential-form" onSubmit={saveCredential}>
      <h3>Add SSH credential</h3>
      <input aria-label="Credential label" placeholder="Label" value={label} onChange={e => setLabel(e.target.value)} required />
      <input aria-label="SSH username" placeholder="Username" value={username} onChange={e => setUsername(e.target.value)} required />
      <select aria-label="Authentication type" value={kind} onChange={e => setKind(e.target.value as typeof kind)}><option value="linux_password">Password</option><option value="linux_ssh_key">Private key</option></select>
      {kind === 'linux_password' ? <input aria-label="SSH password" type="password" autoComplete="new-password" value={secret} onChange={e => setSecret(e.target.value)} required />
        : <textarea aria-label="SSH private key" placeholder="Paste private key" value={secret} onChange={e => setSecret(e.target.value)} required />}
      <button className="button">Save credential</button>
    </form>}
  </div>
}
