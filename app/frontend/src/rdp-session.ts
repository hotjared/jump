import Guacamole from 'guacamole-common-js'
import type { SessionState } from './ssh-session'

const MAX_CLIPBOARD_BYTES = 1024 * 1024
const textMime = /^text\/plain(?:;\s*charset=utf-8)?$/i

export class RdpSession {
  readonly protocol = 'RDP' as const
  state: SessionState = 'connecting'
  error = ''
  clipboardError = ''
  clipboardStatus = ''
  hasRemoteClipboard = false
  private remoteClipboard = ''
  private statusTimer: ReturnType<typeof setTimeout> | null = null
  private listeners = new Set<() => void>()
  private client: any = null

  constructor(public id: string, public deviceId: string, public name: string, public platform: string) {}

  subscribe = (listener: () => void) => {
    this.listeners.add(listener)
    return () => { this.listeners.delete(listener) }
  }
  private notify() { this.listeners.forEach(listener => listener()) }

  attach(node: HTMLDivElement, visible: () => boolean) {
    const url = new URL(`/ws/rdp-sessions/${this.id}`, location.href)
    url.protocol = location.protocol === 'https:' ? 'wss:' : 'ws:'
    const tunnel = new Guacamole.WebSocketTunnel(url.href)
    tunnel.receiveTimeout = 45000
    const client = new Guacamole.Client(tunnel)
    this.client = client
    client.onclipboard = (stream: any, mime: string) => {
      if (!textMime.test(mime)) { client.sendAck(stream.index, 'Unsupported clipboard type', 0x0300); return }
      const reader = new Guacamole.StringReader(stream)
      let value = ''
      let size = 0
      let valid = true
      reader.ontext = (chunk: string) => {
        size += new TextEncoder().encode(chunk).length
        if (size > MAX_CLIPBOARD_BYTES) valid = false
        if (valid) value += chunk
        client.sendAck(stream.index, valid ? 'OK' : 'Clipboard too large', valid ? 0 : 0x0300)
      }
      reader.onend = () => {
        if (!valid || this.client !== client) return
        this.remoteClipboard = value
        this.hasRemoteClipboard = true
        this.notify()
      }
      client.sendAck(stream.index, 'OK', 0)
    }
    const display = client.getDisplay()
    node.appendChild(display.getElement())
    const mouse = new Guacamole.Mouse(display.getElement())
    const keyboard = new Guacamole.Keyboard(document)
    const sendMouse = (state: any) => {
      if (!visible() || this.state !== 'connected') return
      const rect = display.getElement().getBoundingClientRect()
      const scale = rect.width / (display.getWidth() || rect.width || 1)
      client.sendMouseState({ ...state, x: Math.round(state.x / scale), y: Math.round(state.y / scale) })
    }
    mouse.onmousedown = sendMouse
    mouse.onmouseup = sendMouse
    mouse.onmousemove = sendMouse
    keyboard.onkeydown = (key: number) => { if (!visible()) return true; client.sendKeyEvent(1, key); return false }
    keyboard.onkeyup = (key: number) => { if (!visible()) return true; client.sendKeyEvent(0, key); return false }
    client.onstatechange = (state: number) => {
      if (state === 3) this.state = 'connected'
      if (state === 5 && this.state !== 'error') this.state = 'disconnected'
      this.notify()
    }
    client.onerror = (status: { message?: string }) => {
      const safe = [
        'RDP is unavailable on this device. Check that Windows Remote Desktop is listening locally.',
        'RDP authentication failed. Check the saved credential and NLA settings.',
        'RDP protocol service unavailable.', 'RDP connection timed out.',
        'Jump agent disconnected.', 'Browser disconnected.',
        'Update the Jump agent to enable browser RDP.', 'Unsupported RDP credential.',
        'RDP session expired.', 'RDP protocol service disconnected.',
      ]
      this.error = status?.message && safe.includes(status.message) ? status.message
        : 'RDP connection failed. Check the saved credential, local RDP service, and NLA settings.'
      this.state = 'error'; this.notify()
    }
    client.connect()
    let frame = 0
    let last = ''
    const resize = () => {
      if (!visible() || frame) return
      frame = requestAnimationFrame(() => {
        frame = 0
        const width = Math.max(320, Math.min(7680, node.clientWidth))
        const height = Math.max(200, Math.min(4320, node.clientHeight))
        if (!width || !height) return
        const size = `${width}:${height}`
        if (size !== last && this.state === 'connected') { last = size; client.sendSize(width, height) }
        const nativeWidth = display.getWidth()
        const nativeHeight = display.getHeight()
        if (nativeWidth && nativeHeight) display.scale(Math.min(width / nativeWidth, height / nativeHeight))
      })
    }
    const observer = new ResizeObserver(resize)
    display.onresize = resize
    observer.observe(node)
    const unsubscribe = this.subscribe(resize)
    resize()
    return () => {
      if (this.statusTimer) clearTimeout(this.statusTimer)
      this.remoteClipboard = ''; this.hasRemoteClipboard = false
      this.clipboardError = ''; this.clipboardStatus = ''
      observer.disconnect(); unsubscribe(); cancelAnimationFrame(frame)
      display.onresize = null
      keyboard.onkeydown = keyboard.onkeyup = null
      mouse.onmousedown = mouse.onmouseup = mouse.onmousemove = null
      client.disconnect(); this.client = null
      display.getElement().remove()
    }
  }

  disconnect() {
    this.client?.disconnect()
    this.state = 'disconnected'; this.notify()
  }

  private flash(status: string) {
    if (this.statusTimer) clearTimeout(this.statusTimer)
    this.clipboardError = ''
    this.clipboardStatus = status
    this.notify()
    this.statusTimer = setTimeout(() => { this.clipboardStatus = ''; this.notify() }, 2000)
  }

  async pasteLocalClipboardToRemote() {
    if (this.state !== 'connected' || !this.client) return
    try {
      const value = await navigator.clipboard.readText()
      if (this.state !== 'connected' || !this.client) return
      if (new TextEncoder().encode(value).length > MAX_CLIPBOARD_BYTES) {
        this.clipboardError = 'Clipboard text exceeds the 1 MiB limit.'; this.notify(); return
      }
      const writer = new Guacamole.StringWriter(this.client.createClipboardStream('text/plain'))
      writer.onack = (status: { code: number }) => {
        if (status.code !== 0) { this.clipboardError = 'Remote clipboard transfer failed.'; this.clipboardStatus = ''; this.notify() }
      }
      writer.sendText(value)
      writer.sendEnd()
      this.flash('Sent')
    } catch {
      this.clipboardError = 'Could not read your clipboard. Allow clipboard access and try again.'
      this.clipboardStatus = ''; this.notify()
    }
  }

  async copyRemoteClipboardToLocal() {
    if (this.state !== 'connected' || !this.hasRemoteClipboard) return
    try {
      await navigator.clipboard.writeText(this.remoteClipboard)
      this.flash('Copied')
    } catch {
      this.clipboardError = 'Could not write to your clipboard. Allow clipboard access and try again.'
      this.clipboardStatus = ''; this.notify()
    }
  }
}
