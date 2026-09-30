import type { SessionState } from './ssh-session'

type PendingOperation = {
 id: string; kind: string; cancelled: boolean; chunks: Uint8Array[]; size: number; count: number; index: number;
 upload: Uint8Array[]; sent: number; timer: ReturnType<typeof setTimeout>;
 resolve: (text: string) => void; reject: (error: Error) => void;
}
const clipboardMax = 1024 * 1024
const clipboardChunk = 16384
const operationErrors: Record<string, string> = {
 control_required: 'Switch to Control to use this action.', operation_busy: 'A Screen action is already in progress.',
 operation_timeout: 'Screen action timed out.', sas_blocked: 'Windows policy blocked remote Ctrl+Alt+Del.',
 sas_unavailable: 'Windows could not send remote Ctrl+Alt+Del.', clipboard_unavailable: 'The Windows clipboard is unavailable.',
 clipboard_too_large: 'Clipboard text is too large (1 MiB maximum).', invalid_clipboard: 'Invalid Screen clipboard transfer.',
 operation_cancelled: 'Screen action cancelled.',
}
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
  clipboardError = ''
  operationMessage = ''
  operationBusy = false
  private pendingOperation: PendingOperation | null = null
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
    this.socket.onclose = () => { this.rejectOperation('Screen session ended.'); this.closed = true; this.latest = null; if (this.state !== 'error') this.state = 'disconnected'; this.notify() }
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
          this.rejectOperation('Screen session ended.'); this.state = this.error ? 'error' : 'disconnected'; this.closed = true; this.latest = null
        } else if (['screen_clipboard', 'screen_clipboard_ack', 'screen_operation_result'].includes(f.type)) this.operationResponse(f)
        else if (f.type === 'screen_mode' && ['control', 'view'].includes(f.mode)) { this.mode = f.mode; this.changingMode = false }
        else throw new Error()
        this.notify()
      } else throw new Error()
    } catch { this.error = 'Invalid screen frame'; this.disconnect(); this.state = 'error'; this.notify() }
  }
  setMode(mode: 'control' | 'view') {
    if (this.state !== 'connected' || this.changingMode) return
    if (this.pendingOperation) { this.pendingOperation.cancelled = true; this.send({ type: 'screen_operation_cancel', request_id: this.pendingOperation.id }) }
    this.release(); this.changingMode = true
    // Disable input immediately; only the agent acknowledgement can restore control.
    this.mode = 'view'; this.notify(); this.send({ type: 'screen_mode', mode })
  }
  get canControl() { return this.state === 'connected' && this.mode === 'control' && !this.changingMode && !this.closed }
  get hasRemoteClipboard() { return this.canControl && !this.operationBusy }
  private rejectOperation(message: string) {
    const pending = this.pendingOperation
    if (!pending) return
    clearTimeout(pending.timer); this.pendingOperation = null; this.operationBusy = false
    pending.chunks = []; pending.upload = []; pending.reject(new Error(message)); this.notify()
  }
  private operation(kind: string, bytes?: Uint8Array): Promise<string> {
    if (!this.canControl || this.operationBusy) return Promise.reject(new Error('Switch to Control to use this action.'))
    const upload: Uint8Array[] = []
    if (bytes) for (let offset = 0; offset < bytes.length || offset === 0; offset += clipboardChunk) upload.push(bytes.slice(offset, offset + clipboardChunk))
    return new Promise((resolve, reject) => {
      const id = crypto.randomUUID()
      const timer = setTimeout(() => {
        if (this.pendingOperation) this.pendingOperation.cancelled = true
        this.send({ type: 'screen_operation_cancel', request_id: id })
        // Leave transfer state until the agent acknowledges cancellation, so
        // already queued chunks can be validated without poisoning the stream.
        this.clipboardError = 'Screen action timed out.'; this.notify()
      }, 30000)
      this.pendingOperation = { id, kind, cancelled: false, chunks: [], size: 0, count: 0, index: 0, upload, sent: 0, timer, resolve, reject }
      this.operationBusy = true; this.notify()
      this.send({ type: 'screen_operation', request_id: id, kind })
      if (kind === 'clipboard_set') this.sendClipboardChunk()
    })
  }
  private sendClipboardChunk() {
    const op = this.pendingOperation!
    const bytes = op.upload[op.sent]
    let binary = ''; for (const byte of bytes) binary += String.fromCharCode(byte)
    this.send({ type: 'screen_clipboard', request_id: op.id, index: op.sent, count: op.upload.length, data: btoa(binary) })
  }
  private operationResponse(frame: Record<string, unknown>) {
    const op = this.pendingOperation
    if (!op || frame.request_id !== op.id) throw new Error()
    if (op.cancelled && frame.type !== 'screen_operation_result') return
    if (frame.type === 'screen_clipboard_ack') {
      const index = frame.index ?? 0
      if (op.kind !== 'clipboard_set' || index !== op.sent || op.sent >= op.upload.length) throw new Error()
      op.sent++
      if (op.sent < op.upload.length) this.sendClipboardChunk()
    } else if (frame.type === 'screen_clipboard') {
      const index = frame.index ?? 0; const count = frame.count
      if (op.kind !== 'clipboard_get' || index !== op.index || !Number.isInteger(count) || Number(count) < 1 || Number(count) > 64 || op.count && op.count !== count || index >= Number(count) || typeof frame.data !== 'string' || frame.data.length > 21848) throw new Error()
      const binary = atob(frame.data); const bytes = Uint8Array.from(binary, ch => ch.charCodeAt(0))
      if (bytes.length > clipboardChunk || index < Number(count) - 1 && bytes.length !== clipboardChunk || !bytes.length && count !== 1 || op.size + bytes.length > clipboardMax) throw new Error()
      op.chunks.push(bytes); op.size += bytes.length; op.index++; op.count = Number(count)
      this.send({ type: 'screen_clipboard_ack', request_id: op.id, index })
    } else {
      if (frame.kind !== op.kind || typeof frame.code !== 'string' || frame.code !== 'ok' && !(frame.code in operationErrors)) throw new Error()
      if (op.cancelled && frame.code === 'ok') { this.rejectOperation(operationErrors.operation_cancelled); return }
      if (frame.code !== 'ok') { this.rejectOperation(operationErrors[frame.code]); return }
      if (op.kind === 'clipboard_set' && op.sent !== op.upload.length || op.kind === 'clipboard_get' && (!op.count || op.index !== op.count)) throw new Error()
      const data = new Uint8Array(op.size); let offset = 0
      for (const chunk of op.chunks) { data.set(chunk, offset); offset += chunk.length }
      const text = new TextDecoder('utf-8', { fatal: true }).decode(data)
      if (text.includes('\0')) throw new Error()
      clearTimeout(op.timer); this.pendingOperation = null; this.operationBusy = false
      op.chunks = []; op.upload = []; op.resolve(text); this.notify()
    }
  }
  async pasteLocalClipboardToRemote(): Promise<boolean> {
    this.clipboardError = ''; this.notify()
    if (!this.canControl || this.operationBusy) return false
    let text: string
    try { text = await navigator.clipboard.readText() } catch {
      this.clipboardError = 'Could not read your clipboard. Check browser clipboard permission.'; this.notify(); return false
    }
    if (text.length > clipboardMax) { this.clipboardError = operationErrors.clipboard_too_large; this.notify(); return false }
    const bytes = new TextEncoder().encode(text)
    if (bytes.length > clipboardMax || text.includes('\0')) {
      this.clipboardError = bytes.length > clipboardMax ? operationErrors.clipboard_too_large : operationErrors.invalid_clipboard; this.notify(); return false
    }
    try { await this.operation('clipboard_set', bytes); return true } catch (error) {
      this.clipboardError = (error as Error).message; this.notify(); return false
    }
  }
  async copyRemoteClipboardToLocal(): Promise<boolean> {
    this.clipboardError = ''; this.notify()
    if (!this.canControl || this.operationBusy) return false
    let text: string
    try { text = await this.operation('clipboard_get') } catch (error) {
      this.clipboardError = (error as Error).message; this.notify(); return false
    }
    try { await navigator.clipboard.writeText(text); return true } catch {
      this.clipboardError = 'Could not write to your clipboard. Check browser clipboard permission.'; this.notify(); return false
    }
  }
  async sendSAS() {
    this.operationMessage = ''; this.notify()
    if (!this.canControl || this.operationBusy) return
    this.release()
    try { await this.operation('sas'); this.operationMessage = 'Ctrl+Alt+Del request sent.' } catch (error) { this.operationMessage = (error as Error).message }
    this.notify()
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
    this.rejectOperation('Screen session ended.'); this.release(); this.send({ type: 'screen_close' }); this.closed = true; this.latest = null
    this.socket.close(); this.state = 'disconnected'; this.notify()
  }
}
