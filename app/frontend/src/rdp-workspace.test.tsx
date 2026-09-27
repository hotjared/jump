// @vitest-environment jsdom
import { describe, expect, it, vi } from 'vitest'
import { renderToStaticMarkup } from 'react-dom/server'

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
    expect(html).toContain('Disconnect')
    expect(html).toContain('RDP localhost:3389')
  })
})
