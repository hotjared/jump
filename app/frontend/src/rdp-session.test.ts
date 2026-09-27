// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest'

const mocks = vi.hoisted(() => ({ clients: [] as any[], streams: [] as any[] }))
vi.mock('guacamole-common-js', () => ({ default: {
  WebSocketTunnel: class { constructor(_url: string) {} },
  Client: class {
    onstatechange: (state: number) => void = () => {}
    onerror: (status: { message: string }) => void = () => {}
    onclipboard: (stream: any, mime: string) => void = () => {}
    sendAck = vi.fn()
    createClipboardStream() {
      const stream = { index: mocks.streams.length + 1, blobs: [] as string[], ended: false }
      mocks.streams.push(stream)
      return stream
    }
    element = document.createElement('div')
    display = { getElement: () => this.element, getWidth: () => 1280, getHeight: () => 720, scale: () => {}, onresize: null }
    constructor(_tunnel: unknown) { mocks.clients.push(this) }
    getDisplay() { return this.display }
    connect() { this.onstatechange(3) }
    disconnect() { this.onstatechange(5) }
    sendSize() {}
    sendMouseState() {}
    sendKeyEvent() {}
  },
  Mouse: class { constructor(_node: HTMLElement) {} },
  Keyboard: class { constructor(_node: Document) {} },
  StringReader: class {
    ontext: (text: string) => void = () => {}
    onend: () => void = () => {}
    constructor(stream: any) { stream.reader = this }
  },
  StringWriter: class {
    onack: (status: { code: number }) => void = () => {}
    constructor(private stream: any) {}
    sendText(text: string) { this.stream.blobs.push(text) }
    sendEnd() { this.stream.ended = true }
  },
} }))

import { RdpSession } from './rdp-session'

afterEach(() => { vi.unstubAllGlobals(); vi.useRealTimers(); mocks.clients.length = 0; mocks.streams.length = 0 })

function connectedSession() {
  vi.stubGlobal('ResizeObserver', class { observe() {} disconnect() {} })
  vi.stubGlobal('requestAnimationFrame', () => 1)
  vi.stubGlobal('cancelAnimationFrame', () => {})
  const session = new RdpSession('id', 'device', 'Windows', 'windows')
  const detach = session.attach(document.createElement('div'), () => true)
  return { session, client: mocks.clients[0], detach }
}

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

describe('manual text clipboard transfer', () => {
  it('reads local clipboard only on Paste and sends UTF-8 text through a closed clipboard stream', async () => {
    const readText = vi.fn().mockResolvedValue('hello 🌍')
    vi.stubGlobal('navigator', { clipboard: { readText, writeText: vi.fn() } })
    const { session, detach } = connectedSession()
    expect(readText).not.toHaveBeenCalled()
    await session.pasteLocalClipboardToRemote()
    expect(readText).toHaveBeenCalledTimes(1)
    expect(mocks.streams[0]).toMatchObject({ blobs: ['hello 🌍'], ended: true })
    expect(session.clipboardStatus).toBe('Sent')
    detach()
  })

  it('keeps only the latest completed remote text until explicit Copy', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined)
    vi.stubGlobal('navigator', { clipboard: { writeText, readText: vi.fn() } })
    const { session, client, detach } = connectedSession()
    expect(session.hasRemoteClipboard).toBe(false)
    await session.copyRemoteClipboardToLocal()
    expect(writeText).not.toHaveBeenCalled()
    const first: any = { index: 7 }
    client.onclipboard(first, 'image/png')
    expect(session.hasRemoteClipboard).toBe(false)
    client.onclipboard(first, 'text/plain;charset=utf-8')
    first.reader.ontext('first')
    expect(session.hasRemoteClipboard).toBe(false)
    first.reader.onend()
    const second: any = { index: 8 }
    client.onclipboard(second, 'text/plain')
    second.reader.ontext('latest')
    second.reader.onend()
    expect(writeText).not.toHaveBeenCalled()
    await session.copyRemoteClipboardToLocal()
    expect(writeText).toHaveBeenCalledExactlyOnceWith('latest')
    detach()
    expect(session.hasRemoteClipboard).toBe(false)
  })

  it('shows safe errors for clipboard permissions and rejects oversized text', async () => {
    const readText = vi.fn().mockRejectedValue(new Error('private clipboard content'))
    const writeText = vi.fn().mockRejectedValue(new Error('private clipboard content'))
    vi.stubGlobal('navigator', { clipboard: { readText, writeText } })
    const { session, client, detach } = connectedSession()
    await session.pasteLocalClipboardToRemote()
    expect(session.clipboardError).toContain('Could not read')
    expect(session.clipboardError).not.toContain('private clipboard content')
    const stream: any = { index: 9 }
    client.onclipboard(stream, 'text/plain')
    stream.reader.ontext('remote secret')
    stream.reader.onend()
    await session.copyRemoteClipboardToLocal()
    expect(session.clipboardError).toContain('Could not write')
    readText.mockResolvedValue('a'.repeat(1024 * 1024 + 1))
    await session.pasteLocalClipboardToRemote()
    expect(session.clipboardError).toContain('1 MiB')
    expect(mocks.streams).toHaveLength(0)
    const oversized: any = { index: 10 }
    client.onclipboard(oversized, 'text/plain')
    oversized.reader.ontext('x'.repeat(1024 * 1024 + 1))
    oversized.reader.onend()
    expect(session.hasRemoteClipboard).toBe(true)
    expect(client.sendAck).toHaveBeenCalledWith(10, 'Clipboard too large', 0x0300)
    detach()
  })
})
