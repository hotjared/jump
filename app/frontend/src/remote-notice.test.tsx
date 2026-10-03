// @vitest-environment jsdom
import { afterEach, expect, it, vi } from 'vitest'
import { act, cleanup, fireEvent, render, screen } from '@testing-library/react'
import { readFileSync } from 'node:fs'
import RemoteNotice, { useRemoteNotice } from './RemoteNotice'
import SessionWorkspace from './SessionWorkspace'
import { RdpSession } from './rdp-session'
import { ScreenSession } from './screen-session'

vi.mock('@xterm/xterm', () => ({ Terminal: class {} }))
vi.mock('@xterm/addon-fit', () => ({ FitAddon: class {} }))
afterEach(() => { cleanup(); vi.useRealTimers(); vi.unstubAllGlobals() })

function Harness() {
  const { notice, notify, dismiss } = useRemoteNotice()
  return <div className="remote-session-area"><button onClick={() => notify('Sent')}>Send</button><button onClick={() => notify('Failed', 'error')}>Fail</button><RemoteNotice notice={notice} dismiss={dismiss} /></div>
}

it('replaces notices, restarts the timer, and dismisses info/errors automatically', () => {
  vi.useFakeTimers()
  render(<Harness />)
  fireEvent.click(screen.getByText('Send'))
  act(() => vi.advanceTimersByTime(2500))
  fireEvent.click(screen.getByText('Send'))
  act(() => vi.advanceTimersByTime(1000))
  expect(screen.getAllByText('Sent')).toHaveLength(1)
  act(() => vi.advanceTimersByTime(2000))
  expect(screen.queryByText('Sent')).toBeNull()
  fireEvent.click(screen.getByText('Fail'))
  act(() => vi.advanceTimersByTime(6000))
  expect(screen.queryByRole('alert')).toBeNull()
})

it.each(['RDP', 'Screen'] as const)('keeps %s action feedback outside the measured display and preserves connection errors', async protocol => {
  vi.useFakeTimers()
  vi.stubGlobal('WebSocket', class { onmessage = null; onclose = null; onerror = null })
  const session = protocol === 'RDP' ? new RdpSession('id', 'device', 'WIN', 'windows') : new ScreenSession('id', 'device', 'WIN', 'windows', ['screen_control_v2'])
  const attach = vi.spyOn(session, 'attach').mockReturnValue(() => {})
  session.state = 'connected'
  session.error = 'Persistent connection problem'
  session.clipboardError = 'Clipboard action failed'
  render(<SessionWorkspace sessions={[session]} activeId="id" select={() => {}} close={() => {}} />)
  const display = document.querySelector('.workspace-desktop')!
  const area = display.parentElement!
  const toast = document.querySelector('.remote-notice')!
  expect(area.className).toBe('remote-session-area')
  expect(toast.parentElement).toBe(area)
  expect(display.contains(toast)).toBe(false)
  const style = document.createElement('style')
  style.textContent = readFileSync('src/style.css', 'utf8')
  document.head.append(style)
  expect(getComputedStyle(toast).position).toBe('absolute')
  act(() => vi.advanceTimersByTime(6000))
  expect(document.querySelector('.remote-notice')).toBeNull()
  expect(document.querySelector('.workspace-desktop')).toBe(display)
  expect(attach).toHaveBeenCalledOnce()
  expect(screen.getByText('Persistent connection problem')).toBeTruthy()
  style.remove()
})
