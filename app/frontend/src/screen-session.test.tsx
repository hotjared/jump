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
function active(capabilities = ['screen_control_v1', 'screen_control_v2']) {
  const session = new ScreenSession('sid', 'device', 'PC', 'windows', capabilities)
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
    expect(connected.mock.calls[0][0].protocol).toBe('Screen')
    expect(connected.mock.calls[0][0].supportsAdminOperations).toBe(false)
    expect(connected.mock.calls[0][0].capabilities).toEqual(['screen_control_v1']); view.unmount()
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

function opReceive(frame: object) { Socket.last.onmessage?.({ data: JSON.stringify(frame) }) }
function opFrames() { return Socket.last.send.mock.calls.map(([raw]) => JSON.parse(raw)) }
function opRequest() { return opFrames().find(frame => frame.type === 'screen_operation') }

describe('explicit Screen actions', () => {
  it('round trips Unicode and empty text only on explicit requests', async () => {
    const session = active(); const text = 'héllo 世界 😀'
    const readText = vi.fn().mockResolvedValue(text); const writeText = vi.fn().mockResolvedValue(undefined)
    Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { readText, writeText } })
    expect(readText).not.toHaveBeenCalled(); expect(writeText).not.toHaveBeenCalled()
    const paste = session.pasteLocalClipboardToRemote()
    await waitFor(() => expect(opRequest()?.kind).toBe('clipboard_set'))
    const id = opRequest().request_id
    const chunk = opFrames().find(frame => frame.type === 'screen_clipboard')
    expect(new TextDecoder().decode(Uint8Array.from(atob(chunk.data), ch => ch.charCodeAt(0)))).toBe(text)
    opReceive({ type: 'screen_clipboard_ack', request_id: id, index: 0 })
    opReceive({ type: 'screen_operation_result', request_id: id, kind: 'clipboard_set', code: 'ok' })
    expect(await paste).toBe(true)
    for (const data of [chunk.data, '']) {
      Socket.last.send.mockClear(); const copy = session.copyRemoteClipboardToLocal(); const copyId = opRequest().request_id
      opReceive({ type: 'screen_clipboard', request_id: copyId, index: 0, count: 1, data })
      expect(opFrames().at(-1)).toEqual({ type: 'screen_clipboard_ack', request_id: copyId, index: 0 })
      opReceive({ type: 'screen_operation_result', request_id: copyId, kind: 'clipboard_get', code: 'ok' })
      expect(await copy).toBe(true); expect(writeText).toHaveBeenLastCalledWith(data ? text : '')
    }
    session.disconnect()
  })
  it('reports browser permission errors without exposing raw errors or ending Screen', async () => {
    const session = active(); const readText = vi.fn().mockRejectedValue(new Error('sensitive details')); const writeText = vi.fn().mockRejectedValue(new Error('sensitive details'))
    Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { readText, writeText } })
    expect(await session.pasteLocalClipboardToRemote()).toBe(false)
    expect(session.clipboardError).toBe('Could not read your clipboard. Check browser clipboard permission.')
    const copy = session.copyRemoteClipboardToLocal(); const id = opRequest().request_id
    opReceive({ type: 'screen_clipboard', request_id: id, index: 0, count: 1, data: '' })
    opReceive({ type: 'screen_operation_result', request_id: id, kind: 'clipboard_get', code: 'ok' })
    expect(await copy).toBe(false); expect(session.clipboardError).toBe('Could not write to your clipboard. Check browser clipboard permission.')
    expect(session.state).toBe('connected'); session.disconnect()
  })
  it('enforces 1 MiB UTF-8 and one small credited chunk at a time', async () => {
    const session = active(); const readText = vi.fn().mockResolvedValue('x'.repeat(1048577))
    Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { readText } })
    expect(await session.pasteLocalClipboardToRemote()).toBe(false); expect(opRequest()).toBeUndefined()
    expect(session.clipboardError).toContain('1 MiB')
    readText.mockResolvedValue('x'.repeat(1048576)); const paste = session.pasteLocalClipboardToRemote()
    await waitFor(() => expect(opRequest()).toBeTruthy()); const id = opRequest().request_id
    for (let index = 0; index < 64; index++) {
      const chunks = opFrames().filter(frame => frame.type === 'screen_clipboard')
      expect(chunks.length).toBe(index + 1); expect(JSON.stringify(chunks[index]).length).toBeLessThan(24576); expect(chunks[index].count).toBe(64)
      opReceive({ type: 'screen_clipboard_ack', request_id: id, index })
    }
    opReceive({ type: 'screen_operation_result', request_id: id, kind: 'clipboard_set', code: 'ok' })
    expect(await paste).toBe(true); session.disconnect()
  })
  it('rejects malformed/missing/duplicate remote chunks', async () => {
    for (const frame of [{ index: 1, count: 2, data: 'eA==' }, { index: 0, count: 65, data: '' }]) {
      const session = active(); const copy = session.copyRemoteClipboardToLocal(); const id = opRequest().request_id
      opReceive({ type: 'screen_clipboard', request_id: id, ...frame })
      expect(await copy).toBe(false); expect(session.state).toBe('error')
    }
  })
  it('sends dedicated SAS, survives policy denial, and gates all actions in View Only or pending Control', async () => {
    const session = active(); const sas = session.sendSAS(); const id = opRequest().request_id
    expect(opRequest().kind).toBe('sas')
    expect(opFrames().filter(frame => frame.type === 'screen_input').every(frame => frame.input.action === 'release')).toBe(true)
    opReceive({ type: 'screen_operation_result', request_id: id, kind: 'sas', code: 'sas_blocked' })
    await sas; expect(session.operationMessage).toBe('Windows policy blocked remote Ctrl+Alt+Del.'); expect(session.state).toBe('connected')
    session.setMode('view'); opReceive({ type: 'screen_mode', mode: 'view' }); Socket.last.send.mockClear()
    await session.sendSAS(); expect(await session.copyRemoteClipboardToLocal()).toBe(false); expect(Socket.last.send).not.toHaveBeenCalled()
    session.setMode('control'); Socket.last.send.mockClear()
    await session.sendSAS(); expect(await session.pasteLocalClipboardToRemote()).toBe(false); expect(Socket.last.send).not.toHaveBeenCalled()
    opReceive({ type: 'screen_mode', mode: 'control' }); expect(session.canControl).toBe(true); session.disconnect()
  })
  it('shows matching clipboard and SAS buttons disabled in View Only', () => {
    const session = active(); vi.spyOn(session, 'attach').mockReturnValue(() => {})
    const view = render(<SessionWorkspace sessions={[session]} activeId={session.id} select={() => {}} close={() => {}} />)
    for (const label of ['Paste to Remote', 'Copy from Remote', 'Ctrl+Alt+Del']) expect((screen.getByRole('button', { name: label }) as HTMLButtonElement).disabled).toBe(false)
    fireEvent.click(screen.getByRole('button', { name: 'Control · switch to View Only' }))
    for (const label of ['Paste to Remote', 'Copy from Remote', 'Ctrl+Alt+Del']) expect((screen.getByRole('button', { name: label }) as HTMLButtonElement).disabled).toBe(true)
    view.unmount(); session.disconnect()
  })
})

it('revokes input immediately when a mode change cancels clipboard, draining queued replies', async () => {
  const session = active(); const copy = session.copyRemoteClipboardToLocal(); const id = opRequest().request_id
  session.setMode('view')
  expect(opFrames().some(frame => frame.type === 'screen_operation_cancel')).toBe(true)
  expect(session.canControl).toBe(false)
  opReceive({ type: 'screen_clipboard', request_id: id, index: 0, count: 1, data: 'eA==' })
  expect(opFrames().some(frame => frame.type === 'screen_clipboard_ack')).toBe(false)
  opReceive({ type: 'screen_operation_result', request_id: id, kind: 'clipboard_get', code: 'operation_cancelled' })
  expect(await copy).toBe(false)
  opReceive({ type: 'screen_mode', mode: 'view' })
  expect(session.state).toBe('connected'); expect(session.mode).toBe('view'); session.disconnect()
})


it('keeps v1 basic controls while hiding and suppressing v2 operations', async () => {
  const session = active(['screen_control_v1']); vi.spyOn(session, 'attach').mockReturnValue(() => {})
  const readText = vi.fn(); const writeText = vi.fn()
  Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { readText, writeText } })
  const view = render(<SessionWorkspace sessions={[session]} activeId={session.id} select={() => {}} close={() => {}} fileDevices={['device']} />)
  for (const label of ['Paste to Remote', 'Copy from Remote', 'Ctrl+Alt+Del']) expect(screen.queryByRole('button', { name: label })).toBeNull()
  for (const label of ['Files', 'Control · switch to View Only', 'Fullscreen', 'Disconnect']) expect(screen.getByRole('button', { name: label })).toBeTruthy()
  expect(screen.getByText('Update the Windows Jump agent to enable unattended admin controls.')).toBeTruthy()
  expect(session.canControl).toBe(true); expect(session.hasRemoteClipboard).toBe(false)
  await session.sendSAS()
  expect(await session.pasteLocalClipboardToRemote()).toBe(false)
  expect(await session.copyRemoteClipboardToLocal()).toBe(false)
  expect(readText).not.toHaveBeenCalled(); expect(writeText).not.toHaveBeenCalled()
  expect(Socket.last.send).not.toHaveBeenCalled()
  session.input({ action: 'key', key: 65, down: true }, true)
  expect(opFrames().at(-1)?.type).toBe('screen_input')
  view.unmount(); session.disconnect()
})

it('defaults unknown Screen capabilities to basic v1 controls', async () => {
  const session = new ScreenSession('sid', 'device', 'PC', 'windows')
  opReceive({ type: 'status', state: 'active' })
  expect(session.supportsAdminOperations).toBe(false)
  await session.sendSAS(); expect(opRequest()).toBeUndefined()
  session.disconnect()
})
