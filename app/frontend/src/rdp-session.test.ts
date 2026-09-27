// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest'

const mocks = vi.hoisted(() => ({ clients: [] as Array<{ onstatechange: (state: number) => void; onerror: (status: { message: string }) => void }> }))
vi.mock('guacamole-common-js', () => ({ default: {
  WebSocketTunnel: class { constructor(_url: string) {} },
  Client: class {
    onstatechange: (state: number) => void = () => {}
    onerror: (status: { message: string }) => void = () => {}
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
} }))

import { RdpSession } from './rdp-session'

afterEach(() => { vi.unstubAllGlobals(); mocks.clients.length = 0 })

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
