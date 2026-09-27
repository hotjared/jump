// @vitest-environment jsdom
import { describe, expect, it, vi } from 'vitest'
import { renderToStaticMarkup } from 'react-dom/server'
import { fireEvent, render, screen } from '@testing-library/react'

vi.mock('@xterm/xterm', () => ({ Terminal: class {} }))
vi.mock('@xterm/addon-fit', () => ({ FitAddon: class {} }))

import SessionWorkspace from './SessionWorkspace'
import { RdpSession } from './rdp-session'

describe('RDP workspace', () => {
  it('renders a full primary desktop with session tab, fullscreen and disconnect', () => {
    const session = new RdpSession('session-id', 'device-id', 'WIN-SRV01', 'windows')
    session.state = 'connected'
    const html = renderToStaticMarkup(<SessionWorkspace sessions={[session]} activeId={session.id} select={() => {}} close={() => {}} />)
    expect(html).toContain('session-workspace workspace-open')
    expect(html).toContain('session-view')
    expect(html).toContain('workspace-desktop')
    expect(html).toContain('RDP desktop')
    expect(html).toContain('Fullscreen')
    expect(html).toContain('Paste to Remote')
    expect(html).toContain('Copy from Remote</button>')
    expect(html).toMatch(/disabled=""[^>]*>Copy from Remote/)
    expect(html).toContain('Disconnect')
    expect(html).toContain('RDP localhost:3389')
  })

  it('invokes clipboard actions only on explicit clicks and enables remote copy after receipt', () => {
    const session = new RdpSession('session-id', 'device-id', 'WIN-SRV01', 'windows')
    session.state = 'connected'
    vi.spyOn(session, 'attach').mockImplementation(() => () => {})
    const paste = vi.spyOn(session, 'pasteLocalClipboardToRemote').mockResolvedValue()
    const copy = vi.spyOn(session, 'copyRemoteClipboardToLocal').mockResolvedValue()
    const props = { sessions: [session], activeId: session.id, select: () => {}, close: () => {} }
    const view = render(<SessionWorkspace {...props} />)
    expect(screen.getByRole('button', { name: 'Copy from Remote' }).hasAttribute('disabled')).toBe(true)
    expect(paste).not.toHaveBeenCalled()
    expect(copy).not.toHaveBeenCalled()
    fireEvent.click(screen.getByRole('button', { name: 'Paste to Remote' }))
    expect(paste).toHaveBeenCalledTimes(1)
    session.hasRemoteClipboard = true
    view.rerender(<SessionWorkspace {...props} />)
    fireEvent.click(screen.getByRole('button', { name: 'Copy from Remote' }))
    expect(copy).toHaveBeenCalledTimes(1)
    view.unmount()
  })
})
