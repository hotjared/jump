import type { SessionState } from './ssh-session'

type Input = { action: 'move' | 'button' | 'wheel' | 'key' | 'release'; x?: number; y?: number; button?: number; delta?: number; key?: number; down?: boolean }
export function windowsKey(code: string): number | undefined {
  if (/^Key[A-Z]$/.test(code)) return code.charCodeAt(3)
  if (/^Digit[0-9]$/.test(code)) return code.charCodeAt(5)
  if (/^F([1-9]|1[0-9]|2[0-4])$/.test(code)) return 111 + Number(code.slice(1))
  return ({ Backspace: 8, Tab: 9, Enter: 13, ShiftLeft: 160, ShiftRight: 161, ControlLeft: 162, ControlRight: 163,
    AltLeft: 164, AltRight: 165, Escape: 27, Space: 32, PageUp: 33, PageDown: 34, End: 35, Home: 36,
    ArrowLeft: 37, ArrowUp: 38, ArrowRight: 39, ArrowDown: 40, Insert: 45, Delete: 46, MetaLeft: 91, MetaRight: 92,
    Semicolon: 186, Equal: 187, Comma: 188, Minus: 189, Period: 190, Slash: 191, Backquote: 192,
    BracketLeft: 219, Backslash: 220, BracketRight: 221, Quote: 222 } as Record<string, number>)[code]
}

export class ScreenSession {
  readonly protocol = 'Screen' as const
  state: SessionState = 'connecting'
  error = ''
  mode: 'control' | 'view' = 'control'
  private socket: WebSocket
  private listeners = new Set<() => void>()
  private renderFrame: ((bytes: ArrayBuffer) => Promise<void>) | null = null
  private latest: ArrayBuffer | null = null
  private closed = false
  private changingMode = false
  private lastFrame = 0

  constructor(public id: string, public deviceId: string, public name: string, public platform: string) {
    const url = new URL(`/ws/screen-sessions/${id}`, location.href)
    url.protocol = location.protocol === 'https:' ? 'wss:' : 'ws:'
    this.socket = new WebSocket(url)
    this.socket.binaryType = 'arraybuffer'
    this.socket.onmessage = event => { void this.receive(event.data) }
    this.socket.onerror = () => { this.error = 'Could not connect to Screen Control'; this.state = 'error'; this.notify() }
    this.socket.onclose = () => { this.closed = true; this.latest = null; if (this.state !== 'error') this.state = 'disconnected'; this.notify() }
  }
  subscribe = (listener: () => void) => { this.listeners.add(listener); return () => { this.listeners.delete(listener) } }
  private notify() { this.listeners.forEach(listener => listener()) }
  private send(frame: object) { if (!this.closed && this.socket.readyState === WebSocket.OPEN) this.socket.send(JSON.stringify(frame)) }
  private async receive(data: unknown) {
    if (this.closed) return
    try {
      if (data instanceof ArrayBuffer) {
        if (this.state !== 'connected' || data.byteLength < 16 || data.byteLength > 524300) throw new Error()
        const header = new DataView(data)
        const id = Number(header.getBigUint64(0))
        const width = header.getUint16(8); const height = header.getUint16(10)
        if (!Number.isSafeInteger(id) || id <= this.lastFrame || width < 1 || width > 1920 || height < 1 || height > 1080) throw new Error()
        this.lastFrame = id; this.latest = data
        if (this.renderFrame) await this.renderFrame(data)
        this.send({ type: 'screen_ack', frame_id: id })
      } else if (typeof data === 'string') {
        const f = JSON.parse(data)
        if (f.type === 'status' && f.state === 'active') this.state = 'connected'
        else if (f.type === 'status' && f.state === 'closed') {
          this.error = ['session_closed', 'browser_disconnected'].includes(f.code) ? '' : f.message || 'Screen session ended'
          this.state = this.error ? 'error' : 'disconnected'; this.closed = true; this.latest = null
        } else if (f.type === 'screen_mode' && ['control', 'view'].includes(f.mode)) { this.mode = f.mode; this.changingMode = false }
        else throw new Error()
        this.notify()
      } else throw new Error()
    } catch { this.error = 'Invalid screen frame'; this.disconnect(); this.state = 'error'; this.notify() }
  }
  setMode(mode: 'control' | 'view') {
    if (this.state !== 'connected' || this.changingMode) return
    this.release(); this.changingMode = true
    // Disable input immediately; only the agent acknowledgement can restore control.
    this.mode = 'view'; this.notify(); this.send({ type: 'screen_mode', mode })
  }
  release() { if (this.state === 'connected' && this.mode === 'control') this.send({ type: 'screen_input', input: { action: 'release' } }) }
  input(input: Input, active: boolean) {
    if (active && !document.hidden && this.state === 'connected' && this.mode === 'control' && !this.changingMode && this.socket.bufferedAmount < 16384)
      this.send({ type: 'screen_input', input })
  }
  attach(node: HTMLDivElement, visible: () => boolean) {
    const canvas = document.createElement('canvas')
    canvas.tabIndex = 0; canvas.setAttribute('aria-label', `${this.name} Screen Control desktop`)
    Object.assign(canvas.style, { maxWidth: '100%', maxHeight: '100%', width: 'auto', height: 'auto', touchAction: 'none' })
    node.appendChild(canvas)
    let attached = true
    const render = async (bytes: ArrayBuffer) => {
      const view = new DataView(bytes); const width = view.getUint16(8); const height = view.getUint16(10)
      const image = await createImageBitmap(new Blob([bytes.slice(12)], { type: 'image/jpeg' }))
      try {
        if (image.width !== width || image.height !== height) throw new Error()
        if (!attached || this.closed) return
        canvas.width = width; canvas.height = height
        canvas.getContext('2d')?.drawImage(image, 0, 0)
      } finally { image.close() }
    }
    this.renderFrame = render
    if (this.latest) void render(this.latest).catch(() => this.disconnect())
    const coordinates = (event: MouseEvent) => {
      const rect = canvas.getBoundingClientRect()
      return { x: Math.max(0, Math.min(65535, Math.round((event.clientX - rect.left) / Math.max(1, rect.width) * 65535))),
        y: Math.max(0, Math.min(65535, Math.round((event.clientY - rect.top) / Math.max(1, rect.height) * 65535))) }
    }
    let lastMove = 0
    const move = (e: PointerEvent) => { const now = performance.now(); if (now - lastMove < 33) return; lastMove = now; this.input({ action: 'move', ...coordinates(e) }, visible()) }
    const button = (e: PointerEvent, down: boolean) => {
      if (e.button > 2 || !visible()) return
      e.preventDefault(); canvas.focus()
      if (down) canvas.setPointerCapture?.(e.pointerId)
      this.input({ action: 'move', ...coordinates(e) }, visible())
      this.input({ action: 'button', button: e.button, down }, visible())
    }
    canvas.onpointermove = move; canvas.onpointerdown = e => button(e, true); canvas.onpointerup = e => button(e, false)
    canvas.oncontextmenu = e => { e.preventDefault() }
    canvas.onwheel = e => { if (!visible()) return; e.preventDefault(); this.input({ action: 'wheel', delta: e.deltaY > 0 ? -120 : 120 }, visible()) }
    const key = (e: KeyboardEvent, down: boolean) => {
      if (!visible() || this.state !== 'connected' || this.mode !== 'control') return
      const vk = windowsKey(e.code); if (vk === undefined) return
      e.preventDefault(); this.input({ action: 'key', key: vk, down }, visible())
    }
    canvas.onkeydown = e => key(e, true); canvas.onkeyup = e => key(e, false)
    canvas.onblur = () => this.release(); canvas.onpointercancel = () => this.release()
    const hidden = () => { if (document.hidden) this.release() }
    document.addEventListener('visibilitychange', hidden)
    return () => { attached = false; this.release(); document.removeEventListener('visibilitychange', hidden); if (this.renderFrame === render) this.renderFrame = null; canvas.remove() }
  }
  disconnect() {
    this.release(); this.send({ type: 'screen_close' }); this.closed = true; this.latest = null
    this.socket.close(); this.state = 'disconnected'; this.notify()
  }
}
