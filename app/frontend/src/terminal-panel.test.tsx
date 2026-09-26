import { describe, expect, it, vi } from 'vitest'
import { renderToStaticMarkup } from 'react-dom/server'

// xterm owns browser globals; the panel's initial eligibility states can be
// rendered without a DOM or opening a terminal session.
vi.mock('@xterm/xterm', () => ({ Terminal: class {} }))
vi.mock('@xterm/addon-fit', () => ({ FitAddon: class {} }))

import TerminalPanel from './TerminalPanel'

const base = { id: 'device', online: true, os_family: 'linux', capabilities: ['ssh', 'ssh_terminal_v1'], ssh_host_key: null }
async function mutate<T>(): Promise<T> { throw new Error('Unexpected mutation during server render') }

describe('terminal eligibility', () => {
  it('disables connection when the device is offline', () => {
    const html = renderToStaticMarkup(<TerminalPanel device={{ ...base, online: false }} admin mutate={mutate} />)
    expect(html).toContain('Offline. Connect the agent')
    expect(html).toMatch(/>Connect<\/button>/)
    expect(html).toMatch(/disabled=""[^>]*>Connect<\/button>/)
  })

  it('does not offer browser SSH to an older online agent advertising only ssh', () => {
    const html = renderToStaticMarkup(<TerminalPanel device={{ ...base, capabilities: ['ssh'] }} admin mutate={mutate} />)
    expect(html).toContain('Update the Jump agent to enable browser SSH.')
    expect(html).not.toContain('>Connect</button>')
    expect(html).not.toContain('Add SSH credential')
  })

  it('requires the protocol capability and admin role', () => {
    expect(renderToStaticMarkup(<TerminalPanel device={{ ...base, capabilities: [] }} admin mutate={mutate} />)).toContain('Update the Jump agent')
    expect(renderToStaticMarkup(<TerminalPanel device={{ ...base, os_family: 'windows' }} admin mutate={mutate} />)).toContain('does not advertise Linux SSH')
    expect(renderToStaticMarkup(<TerminalPanel device={base} admin={false} mutate={mutate} />)).toContain('Admin access required')
  })
})
