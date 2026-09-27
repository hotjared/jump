// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import App from './App'

const terminalMocks = vi.hoisted(() => ({
  created: 0, disposed: 0, fits: 0, writes: [] as string[],
  instances: [] as Array<{ cols: number; rows: number }>,
}))
vi.mock('@xterm/xterm', () => ({
  Terminal: class {
    cols = 80; rows = 24
    constructor() { terminalMocks.created++; terminalMocks.instances.push(this) }
    loadAddon() {}
    open() {}
    onData() { return { dispose() {} } }
    write(data: Uint8Array) { terminalMocks.writes.push(new TextDecoder().decode(data)) }
    focus() {}
    dispose() { terminalMocks.disposed++ }
  },
}))
vi.mock('@xterm/addon-fit', () => ({ FitAddon: class { fit() { terminalMocks.fits++ } } }))

const resizeObservers: FakeResizeObserver[] = []
class FakeResizeObserver {
  constructor(private callback: ResizeObserverCallback) { resizeObservers.push(this) }
  observe() {}
  disconnect() {}
  trigger() { this.callback([], this as unknown as ResizeObserver) }
}

class FakeSocket {
  static OPEN = 1
  static instances: FakeSocket[] = []
  readyState = 1
  onmessage: ((event: { data: string }) => void) | null = null
  onerror: (() => void) | null = null
  onclose: (() => void) | null = null
  sent: string[] = []
  constructor(public url: string) { FakeSocket.instances.push(this) }
  send(message: string) { this.sent.push(message) }
  close() { this.readyState = 3; this.onclose?.() }
  emit(frame: object) { this.onmessage?.({ data: JSON.stringify(frame) }) }
}

const makeDevice = (id: string, capabilities = ['ssh', 'ssh_terminal_v1']) => ({
  id, device_uuid: id, hostname: id, display_name: null, os_family: 'linux', os_version: '26',
  architecture: 'amd64', agent_version: '1', capabilities, addresses: [], primary_ip: null,
  current_user: null, group: null, tags: [], online: true, identity_state: 'active',
  last_seen_at: null, enrolled_at: '2026-01-01', ssh_host_key: 'SHA256:trusted',
  agent_update: { update_available: false },
})

let devices = [makeDevice('docker01'), makeDevice('web01')]
let nextSession = 0
const requests: string[] = []
function response(data: unknown) { return { ok: true, json: async () => data } }

beforeEach(() => {
  FakeSocket.instances = []; nextSession = 0; requests.length = 0
  terminalMocks.created = 0; terminalMocks.disposed = 0; terminalMocks.fits = 0; terminalMocks.writes = []; terminalMocks.instances = []
  resizeObservers.length = 0
  devices = [makeDevice('docker01'), makeDevice('web01')]
  vi.stubGlobal('WebSocket', FakeSocket)
  vi.stubGlobal('ResizeObserver', FakeResizeObserver)
  vi.spyOn(HTMLElement.prototype, 'getClientRects').mockReturnValue({ length: 1 } as DOMRectList)
  vi.stubGlobal('requestAnimationFrame', (callback: FrameRequestCallback) => { callback(0); return 1 })
  vi.stubGlobal('cancelAnimationFrame', () => {})
  vi.stubGlobal('fetch', vi.fn(async (input: string, init?: RequestInit) => {
    requests.push(input)
    if (input === '/api/me') return response({ id: 'admin', email: 'admin@example.com', display_name: 'Admin', role: 'admin', csrf: 'csrf' })
    if (input === '/api/devices') return response(devices)
    if (input === '/api/quick-connect-preferences') return response([])
    if (input === '/api/groups' || input === '/api/tags' || input === '/api/audit') return response([])
    if (input.endsWith('/credentials') && !init?.method) return response([{ id: 'cred', label: 'Owner', username: 'owner', kind: 'linux_password' }])
    if (input.endsWith('/quick-connect-preferences') && init?.method === 'PUT') return response({ device_id: 'docker01', protocol: 'ssh', credential_id: 'cred', preferred: true })
    if (input.endsWith('/ssh-sessions') && init?.method === 'POST') return response({ id: `ssh-${++nextSession}` })
    throw new Error(`Unexpected request: ${input}`)
  }))
})
afterEach(() => { cleanup(); vi.unstubAllGlobals(); vi.restoreAllMocks() })

async function openDevice(id: string) {
  await waitFor(() => expect(Array.from(document.querySelectorAll('.device-row')).some(row => row.textContent?.includes(id))).toBe(true))
  fireEvent.click(Array.from(document.querySelectorAll('.device-row')).find(row => row.textContent?.includes(id))!)
  fireEvent.click(screen.getByRole('button', { name: 'Terminal' }))
}

async function connectDevice(id: string) {
  await openDevice(id)
  await screen.findByRole('option', { name: 'Owner (owner)' })
  fireEvent.change(await screen.findByRole('combobox', { name: 'SSH credential' }), { target: { value: 'cred' } })
  const count = FakeSocket.instances.length
  fireEvent.click(screen.getByRole('button', { name: 'Connect' }))
  await waitFor(() => expect(FakeSocket.instances.length).toBe(count + 1))
  const socket = FakeSocket.instances.at(-1)!
  socket.emit({ type: 'status', state: 'active', fingerprint: 'SHA256:trusted' })
  await screen.findByRole('tab', { name: new RegExp(id) })
  await waitFor(() => expect(screen.queryByText('DEVICE DETAILS')).toBeNull())
  return socket
}

describe('session workspace', () => {

  it('starts Quick Connect in the background and opens only when its tab is clicked', async () => {
    render(<App />)
    await screen.findAllByRole('button', { name: 'Connect SSH' })
    fireEvent.click(screen.getAllByRole('button', { name: 'Connect SSH' })[0])
    fireEvent.change(await screen.findByRole('combobox', { name: 'Quick Connect credential' }), { target: { value: 'cred' } })
    fireEvent.click(screen.getByRole('button', { name: 'Connect' }))
    await waitFor(() => expect(FakeSocket.instances.length).toBe(1))
    FakeSocket.instances[0].emit({ type: 'status', state: 'active', fingerprint: 'SHA256:trusted' })
    const tab = await screen.findByRole('tab', { name: /docker01 SSH/ })
    expect(tab.getAttribute('aria-selected')).toBe('false')
    expect(screen.getByRole('heading', { name: /Devices/ })).toBeTruthy()
    expect(document.querySelector('.content')?.hasAttribute('hidden')).toBe(false)
    fireEvent.click(tab)
    expect(tab.getAttribute('aria-selected')).toBe('true')
    expect(document.querySelector('.content')?.hasAttribute('hidden')).toBe(true)
  })

  it('deduplicates terminal resizes, ignores hidden tabs, and resizes once when shown again', async () => {
    render(<App />)
    const socket = await connectDevice('docker01')
    const resizeFrames = () => socket.sent.map(message => JSON.parse(message)).filter(frame => frame.type === 'session_resize')

    await waitFor(() => expect(resizeFrames().length).toBeGreaterThan(0))
    const initial = resizeFrames().length
    resizeObservers.at(-1)!.trigger()
    resizeObservers.at(-1)!.trigger()
    expect(resizeFrames()).toHaveLength(initial)

    terminalMocks.instances.at(-1)!.rows = 30
    resizeObservers.at(-1)!.trigger()
    expect(resizeFrames()).toHaveLength(initial + 1)
    expect(resizeFrames().at(-1)).toMatchObject({ columns: 80, rows: 30 })

    fireEvent.click(screen.getByRole('button', { name: /Devices$/ }))
    terminalMocks.instances.at(-1)!.rows = 31
    resizeObservers.at(-1)!.trigger()
    expect(resizeFrames()).toHaveLength(initial + 1)

    fireEvent.click(screen.getByRole('tab', { name: /docker01/ }))
    expect(resizeFrames()).toHaveLength(initial + 2)
    expect(resizeFrames().at(-1)).toMatchObject({ columns: 80, rows: 31 })
  })

  it('moves a connected terminal from the drawer into a large persistent tab, preserves two buffers through navigation, and disconnects on close', async () => {
    render(<App />)
    const first = await connectDevice('docker01')
    expect(screen.queryByRole('combobox', { name: 'SSH credential' })).toBeNull()
    expect(document.querySelector('.workspace-terminal')).not.toBeNull()
    first.emit({ type: 'session_data', data: btoa('first buffer') })
    await waitFor(() => expect(terminalMocks.writes).toContain('first buffer'))

    fireEvent.click(screen.getByRole('button', { name: /Devices$/ }))
    expect(screen.getByRole('tab', { name: /docker01/ })).toBeTruthy()
    expect(terminalMocks.disposed).toBe(0)
    const second = await connectDevice('web01')
    expect(screen.getAllByRole('tab').length).toBe(2)
    const initialCount = terminalMocks.created
    fireEvent.click(screen.getByRole('tab', { name: /docker01/ }))
    expect(within(screen.getByRole('tabpanel', { name: /docker01/ })).getByLabelText('docker01 SSH terminal')).toBeTruthy()
    expect(terminalMocks.created).toBe(initialCount)
    fireEvent.click(screen.getByRole('button', { name: /Dashboard$/ }))
    expect(screen.getByRole('heading', { name: 'Dashboard' })).toBeTruthy()
    expect(terminalMocks.disposed).toBe(0)
    fireEvent.click(screen.getByRole('tab', { name: /web01/ }))
    expect(second.readyState).toBe(1)
    expect(terminalMocks.created).toBe(initialCount)
    fireEvent.click(screen.getByRole('button', { name: 'Close web01 SSH session' }))
    expect(second.sent.some(message => JSON.parse(message).type === 'session_close')).toBe(true)
    expect(second.readyState).toBe(3)
    expect(first.readyState).toBe(1)
    expect(screen.getByRole('tab', { name: /docker01/ })).toBeTruthy()
  })

  it('opens an existing device session without a duplicate connection', async () => {
    render(<App />)
    await connectDevice('docker01')
    fireEvent.click(screen.getByRole('button', { name: /Devices$/ }))
    await openDevice('docker01')
    expect(screen.getByText('SSH session active')).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: 'Open session' }))
    expect(screen.getByRole('tabpanel', { name: /docker01/ })).toBeTruthy()
    expect(nextSession).toBe(1)
  })

  it('keeps host-key controls and blocks agents missing the SSH protocol capability', async () => {
    devices = [makeDevice('old-agent', ['ssh']), makeDevice('docker01')]
    render(<App />)
    await openDevice('old-agent')
    expect(screen.getByText('Update the Jump agent to enable browser SSH.')).toBeTruthy()
    expect(screen.queryByRole('button', { name: 'Connect' })).toBeNull()
    fireEvent.click(screen.getByRole('button', { name: 'Close' }))
    await openDevice('docker01')
    expect(screen.getByText('SHA256:trusted')).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Reset trusted key' })).toBeTruthy()
    expect(requests.some(url => url.endsWith('/ssh-sessions'))).toBe(false)
  })
})
