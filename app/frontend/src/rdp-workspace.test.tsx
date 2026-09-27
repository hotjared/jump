// @vitest-environment jsdom
import { describe, expect, it, vi } from 'vitest'
import { renderToStaticMarkup } from 'react-dom/server'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'

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
    expect(html).toContain('Copy from Remote')
    expect(html).toMatch(/disabled=""[^>]*>Copy from Remote/)
    expect(html).toContain('Disconnect')
    expect(html).toContain('RDP localhost:3389')
  })

  it('keeps clipboard actions disabled until connected and remote text is available', async () => {
    const session = new RdpSession('id', 'device', 'WIN', 'windows')
    vi.spyOn(session, 'attach').mockReturnValue(() => {})
    const paste = vi.spyOn(session, 'pasteLocalClipboardToRemote').mockResolvedValue(true)
    const copy = vi.spyOn(session, 'copyRemoteClipboardToLocal').mockResolvedValue(true)
    const { unmount } = render(<SessionWorkspace sessions={[session]} activeId={session.id} select={() => {}} close={() => {}} />)
    const pasteButton = screen.getByRole('button', { name: 'Paste to Remote' }) as HTMLButtonElement
    const copyButton = screen.getByRole('button', { name: 'Copy from Remote' }) as HTMLButtonElement
    expect(pasteButton.disabled).toBe(true)
    expect(copyButton.disabled).toBe(true)
    session.state = 'connected'; session.hasRemoteClipboard = true
    // Re-rendering reflects the new session state.
    unmount()
    const view = render(<SessionWorkspace sessions={[session]} activeId={session.id} select={() => {}} close={() => {}} />)
    expect((screen.getByRole('button', { name: 'Copy from Remote' }) as HTMLButtonElement).disabled).toBe(false)
    fireEvent.click(screen.getByRole('button', { name: 'Paste to Remote' }))
    fireEvent.click(screen.getByRole('button', { name: 'Copy from Remote' }))
    await waitFor(() => { expect(paste).toHaveBeenCalledOnce(); expect(copy).toHaveBeenCalledOnce() })
    view.unmount()
  })
})
