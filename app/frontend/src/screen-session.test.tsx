// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { ScreenSession, windowsKey } from './screen-session'
import ScreenPanel from './ScreenPanel'
import SessionWorkspace from './SessionWorkspace'

class Socket {
  static OPEN = 1
  static last: Socket
  readyState = 1
  bufferedAmount = 0
  binaryType = ''
  onmessage: ((event: { data: unknown }) => void) | null = null
  onclose: (() => void) | null = null
  onerror: (() => void) | null = null
  send = vi.fn()
  close = vi.fn()
  constructor() { Socket.last = this }
}
vi.stubGlobal('WebSocket', Socket)
vi.stubGlobal('PointerEvent', MouseEvent)
vi.mock('@xterm/xterm', () => ({ Terminal: class {} }))
vi.mock('@xterm/addon-fit', () => ({ FitAddon: class {} }))
afterEach(() => vi.restoreAllMocks())
function active() {
  const session = new ScreenSession('sid', 'device', 'PC', 'windows')
  Socket.last.onmessage?.({ data: JSON.stringify({ type: 'status', state: 'active' }) })
  return session
}

describe('screen control', () => {
  it('gates capability and starts a credential-free screen session', async () => {
    const device = { id: 'device', hostname: 'PC', display_name: null, online: true, os_family: 'windows', capabilities: [] as string[] }
    const mutate = vi.fn().mockResolvedValue({ id: 'screen' }); const connected = vi.fn()
    const view = render(<ScreenPanel device={device} admin mutate={mutate} onConnected={connected} openExisting={() => {}} />)
    expect(screen.getByText('Update the Windows Jump agent service to enable Screen Control.')).toBeTruthy()
    expect(screen.queryByRole('button', { name: 'Screen Control' })).toBeNull()
    view.rerender(<ScreenPanel device={{ ...device, capabilities: ['screen_control_v1'] }} admin mutate={mutate} onConnected={connected} openExisting={() => {}} />)
    fireEvent.click(screen.getByRole('button', { name: 'Screen Control' }))
    await waitFor(() => expect(connected).toHaveBeenCalledOnce())
    expect(mutate).toHaveBeenCalledWith('/api/devices/device/screen-sessions', 'POST', {})
    expect(connected.mock.calls[0][0].protocol).toBe('Screen'); view.unmount()
  })
  it('renders Screen workspace, Files, mode, fullscreen and disconnect', () => {
    const session = active(); vi.spyOn(session, 'attach').mockReturnValue(() => {})
    const files = vi.fn(); const close = vi.fn(); const full = vi.fn()
    const view = render(<SessionWorkspace sessions={[session]} activeId={session.id} select={() => {}} close={close} fileDevices={['device']} openFiles={files} />)
    expect(screen.getByRole('tab').textContent).toContain('Screen')
    fireEvent.click(screen.getByRole('button', { name: 'Files' })); expect(files).toHaveBeenCalledWith('device')
    Object.defineProperty(view.container.querySelector('.session-view'), 'requestFullscreen', { value: full })
    fireEvent.click(screen.getByRole('button', { name: 'Fullscreen' })); expect(full).toHaveBeenCalledOnce()
    fireEvent.click(screen.getByRole('button', { name: 'Disconnect' })); expect(close).toHaveBeenCalledWith('sid'); view.unmount()
  })
  it('renders bounded frame, acknowledges it and closes image allocations', async () => {
    const session = active(); const image = { width: 2, height: 1, close: vi.fn() }
    const drawImage = vi.fn()
    vi.stubGlobal('createImageBitmap', vi.fn().mockResolvedValue(image))
    vi.spyOn(HTMLCanvasElement.prototype, 'getContext').mockReturnValue({ drawImage } as unknown as CanvasRenderingContext2D)
    const node = document.createElement('div'); const detach = session.attach(node, () => true)
    const bytes = new ArrayBuffer(20); const header = new DataView(bytes)
    header.setBigUint64(0, 1n); header.setUint16(8, 2); header.setUint16(10, 1)
    Socket.last.onmessage?.({ data: bytes })
    await waitFor(() => expect(drawImage).toHaveBeenCalledOnce())
    expect(image.close).toHaveBeenCalledOnce()
    await waitFor(() => expect(Socket.last.send).toHaveBeenCalledWith(JSON.stringify({ type: 'screen_ack', frame_id: 1 })))
    detach(); session.disconnect()
  })
  it('sends focused keyboard/mouse and suppresses inactive, disconnected and View Only input', () => {
    const session = active(); const node = document.createElement('div'); document.body.appendChild(node)
    let visible = true; const detach = session.attach(node, () => visible)
    const canvas = node.querySelector('canvas')!
    fireEvent.keyDown(canvas, { code: 'KeyA' })
    expect(Socket.last.send).toHaveBeenLastCalledWith(JSON.stringify({ type: 'screen_input', input: { action: 'key', key: 65, down: true } }))
    fireEvent.pointerDown(canvas, { button: 0, clientX: 20, clientY: 10 })
    expect(Socket.last.send).toHaveBeenLastCalledWith(JSON.stringify({ type: 'screen_input', input: { action: 'button', button: 0, down: true } }))
    fireEvent.wheel(canvas, { deltaY: 120 })
    expect(Socket.last.send).toHaveBeenLastCalledWith(JSON.stringify({ type: 'screen_input', input: { action: 'wheel', delta: -120 } }))
    Socket.last.send.mockClear(); visible = false
    fireEvent.keyDown(canvas, { code: 'KeyB' }); fireEvent.wheel(canvas, { deltaY: 120 }); expect(Socket.last.send).not.toHaveBeenCalled()
    visible = true; session.setMode('view'); Socket.last.send.mockClear()
    fireEvent.keyDown(canvas, { code: 'KeyB' }); session.input({ action: 'button', button: 0, down: true }, true); expect(Socket.last.send).not.toHaveBeenCalled()
    Socket.last.onmessage?.({ data: JSON.stringify({ type: 'screen_mode', mode: 'view' }) })
    session.setMode('control'); Socket.last.send.mockClear(); session.input({ action: 'key', key: 65, down: true }, true); expect(Socket.last.send).not.toHaveBeenCalled()
    Socket.last.onmessage?.({ data: JSON.stringify({ type: 'screen_mode', mode: 'control' }) })
    session.input({ action: 'key', key: 65, down: true }, true); expect(Socket.last.send).toHaveBeenCalledOnce()
    session.disconnect(); Socket.last.send.mockClear(); fireEvent.keyDown(canvas, { code: 'KeyB' }); expect(Socket.last.send).not.toHaveBeenCalled()
    detach(); node.remove()
  })
  it('maps modifiers, punctuation, navigation and function keys without shell operations', () => {
    expect(windowsKey('ShiftRight')).toBe(161); expect(windowsKey('MetaLeft')).toBe(91)
    expect(windowsKey('Quote')).toBe(222); expect(windowsKey('F12')).toBe(123)
    expect(windowsKey('Delete')).toBe(46); expect(windowsKey('unknown')).toBeUndefined()
  })
})
