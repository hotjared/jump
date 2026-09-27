export type SessionState = 'connecting' | 'connected' | 'disconnected' | 'error'

type Frame = { type: string; data?: string; state?: string; code?: string; message?: string; fingerprint?: string }

// A connection belongs to the application shell, never to the device drawer or
// a particular page. Output received before xterm mounts is replayed on attach.
export class SshSession {
  readonly protocol = 'SSH' as const
  state: SessionState = 'connecting'
  error = ''
  fingerprint: string | null = null
  private socket: WebSocket
  private listeners = new Set<() => void>()
  private output: Uint8Array[] = []
  private writer: ((data: Uint8Array) => void) | null = null

  constructor(public id: string, public deviceId: string, public name: string, public platform: string) {
    const url = new URL(`/ws/sessions/${id}`, location.href)
    url.protocol = location.protocol === 'https:' ? 'wss:' : 'ws:'
    this.socket = new WebSocket(url)
  }

  subscribe = (listener: () => void) => {
    this.listeners.add(listener)
    return () => { this.listeners.delete(listener) }
  }

  private notify() { this.listeners.forEach(listener => listener()) }

  async ready(): Promise<void> {
    return new Promise((resolve, reject) => {
      let pending = true
      const fail = (message: string) => {
        if (pending) { pending = false; reject(new Error(message)) }
      }
      this.socket.onmessage = event => {
        const frame = JSON.parse(event.data as string) as Frame
        if (frame.type === 'session_data' && frame.data) {
          const bytes = Uint8Array.from(atob(frame.data), char => char.charCodeAt(0))
          if (this.writer) this.writer(bytes)
          else this.output.push(bytes)
        } else if (frame.type === 'status') {
          if (frame.state === 'active') {
            this.state = 'connected'; this.fingerprint = frame.fingerprint || null
            this.notify()
            if (pending) { pending = false; resolve() }
          } else if (frame.state === 'closed') {
            this.error = frame.code && frame.code !== 'session_closed' ? frame.message || 'Session disconnected' : ''
            this.state = this.error ? 'error' : 'disconnected'
            this.notify(); fail(this.error || 'Session disconnected')
          }
        }
      }
      this.socket.onerror = () => {
        this.error = 'Could not connect to the session gateway'
        this.state = 'error'; this.notify(); fail(this.error)
      }
      this.socket.onclose = () => {
        if (this.state === 'connecting') fail('Session disconnected before connecting')
        if (this.state === 'connected') this.state = 'disconnected'
        this.notify()
      }
    })
  }

  attach(writer: (data: Uint8Array) => void) {
    this.writer = writer
    this.output.forEach(writer)
    this.output = []
    return () => { this.writer = null }
  }

  sendData(data: string) {
    if (this.state !== 'connected' || this.socket.readyState !== WebSocket.OPEN) return
    const bytes = new TextEncoder().encode(data)
    let binary = ''
    bytes.forEach(byte => { binary += String.fromCharCode(byte) })
    this.socket.send(JSON.stringify({ type: 'session_data', data: btoa(binary) }))
  }

  resize(columns: number, rows: number) {
    if (this.state === 'connected' && this.socket.readyState === WebSocket.OPEN)
      this.socket.send(JSON.stringify({ type: 'session_resize', columns, rows }))
  }

  disconnect() {
    if (this.socket.readyState === WebSocket.OPEN)
      this.socket.send(JSON.stringify({ type: 'session_close' }))
    this.socket.close()
    this.state = 'disconnected'
    this.notify()
  }
}
