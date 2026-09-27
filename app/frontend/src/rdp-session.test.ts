// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest'

const mocks = vi.hoisted(() => ({ clients: [] as Array<any>, streams: [] as Array<any> }))
vi.mock('guacamole-common-js', () => ({ default: {
  WebSocketTunnel: class { constructor(_url: string) {} },
  Client: class {
    onstatechange: (state: number) => void = () => {}
    onerror: (status: { message: string }) => void = () => {}
    onclipboard: (stream: any, mimetype: string) => void = () => {}
    element = document.createElement('div')
    display = { getElement: () => this.element, getWidth: () => 1280, getHeight: () => 720, scale: () => {}, onresize: null }
    constructor(_tunnel: unknown) { mocks.clients.push(this) }
    getDisplay() { return this.display }
    connect() { this.onstatechange(3) }
    disconnect() { this.onstatechange(5) }
    sendSize() {}
    sendMouseState() {}
    sendKeyEvent() {}
    createClipboardStream(mimetype: string) {
      const stream = { mimetype, sent: [] as string[], ended: false, sendBlob(data: string) { this.sent.push(data) }, sendEnd() { this.ended = true } }
      mocks.streams.push(stream)
      return stream
    }
  },
  StringWriter: class {
    constructor(private stream: any) {}
    sendText(value: string) { this.stream.sendBlob(btoa(value)) }
    sendEnd() { this.stream.sendEnd() }
  },
  StringReader: class {
    ontext: (value: string) => void = () => {}
    onend: () => void = () => {}
    constructor(stream: any) {
      stream.receive = (value: string) => this.ontext(value)
      stream.end = () => this.onend()
    }
  },
  Mouse: class { constructor(_node: HTMLElement) {} },
  Keyboard: class { constructor(_node: Document) {} },
} }))

import { RdpSession } from './rdp-session'

afterEach(() => { vi.unstubAllGlobals(); mocks.clients.length = 0; mocks.streams.length = 0 })

describe('RDP connection state', () => {
  it('connects, reports sanitized failure, and disconnects cleanly', () => {
    vi.stubGlobal('ResizeObserver', class { observe() {} disconnect() {} })
    vi.stubGlobal('requestAnimationFrame', () => 1)
    vi.stubGlobal('cancelAnimationFrame', () => {})
    const session = new RdpSession('id', 'device', 'Windows', 'windows')
    expect(session.state).toBe('connecting')
    const node = document.createElement('div')
    const detach = session.attach(node, () => true)
    expect(session.state).toBe('connected')
    expect(node.firstElementChild).toBeTruthy()
    mocks.clients[0].onerror({ message: 'RDP authentication failed. Check the saved credential and NLA settings.' })
    expect(session.state).toBe('error')
    expect(session.error).toContain('authentication failed')
    detach()
    expect(node.firstElementChild).toBeNull()
    session.disconnect()
    expect(session.state).toBe('disconnected')
  })
})

describe('explicit RDP text clipboard', () => {
  function connected() {
    vi.stubGlobal('ResizeObserver', class { observe() {} disconnect() {} })
    vi.stubGlobal('requestAnimationFrame', () => 1)
    vi.stubGlobal('cancelAnimationFrame', () => {})
    const session = new RdpSession('id', 'device', 'Windows', 'windows')
    const detach = session.attach(document.createElement('div'), () => true)
    return { session, detach, client: mocks.clients[0] }
  }

  it('reads locally only when invoked, sends a text stream and ends it', async () => {
    const readText = vi.fn().mockResolvedValue('hello')
    vi.stubGlobal('navigator', { clipboard: { readText } })
    const { session, detach } = connected()
    expect(readText).not.toHaveBeenCalled()
    expect(await session.pasteLocalClipboardToRemote()).toBe(true)
    expect(readText).toHaveBeenCalledOnce()
    expect(mocks.streams[0]).toMatchObject({ mimetype: 'text/plain', sent: [btoa('hello')], ended: true })
    detach()
  })

  it('stores the latest remote text in memory and writes only when invoked', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined)
    vi.stubGlobal('navigator', { clipboard: { writeText } })
    const { session, detach, client } = connected()
    expect(session.hasRemoteClipboard).toBe(false)
    const stream = { sendAck: vi.fn() } as any
    client.onclipboard(stream, 'text/plain;charset=utf-8')
    stream.receive('first'); stream.end()
    expect(session.hasRemoteClipboard).toBe(true)
    expect(writeText).not.toHaveBeenCalled()
    const next = { sendAck: vi.fn() } as any
    client.onclipboard(next, 'text/plain')
    next.receive('latest'); next.end()
    expect(await session.copyRemoteClipboardToLocal()).toBe(true)
    expect(writeText).toHaveBeenCalledWith('latest')
    detach()
    expect(session.hasRemoteClipboard).toBe(false)
  })

  it('ignores non-text and reports browser permission failures without exposing content', async () => {
    const readText = vi.fn().mockRejectedValue(new Error('secret read failure'))
    const writeText = vi.fn().mockRejectedValue(new Error('secret write failure'))
    vi.stubGlobal('navigator', { clipboard: { readText, writeText } })
    const { session, detach, client } = connected()
    const invalid = { sendAck: vi.fn() } as any
    client.onclipboard(invalid, 'image/png')
    expect(invalid.sendAck).toHaveBeenCalled()
    expect(session.hasRemoteClipboard).toBe(false)
    expect(await session.pasteLocalClipboardToRemote()).toBe(false)
    expect(session.clipboardError).not.toContain('secret')
    const stream = { sendAck: vi.fn() } as any
    client.onclipboard(stream, 'text/plain')
    stream.receive('private'); stream.end()
    expect(await session.copyRemoteClipboardToLocal()).toBe(false)
    expect(session.clipboardError).not.toContain('private')
    detach()
  })
})
