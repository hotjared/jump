import { describe, expect, it, vi } from 'vitest'
import { renderToStaticMarkup } from 'react-dom/server'
import AgentUpdatePanel, { type UpdateInfo } from './AgentUpdatePanel'

const base: UpdateInfo = {
  current_version: 'v0.1.2', latest_version: 'v0.1.3', update_available: true,
  remote_update_supported: true, update_state: null,
}
const render = (info: UpdateInfo, admin = true) => renderToStaticMarkup(
  <AgentUpdatePanel info={info} name="PROD-SRV01" online active admin={admin} busy={false} update={vi.fn()} />)

describe('agent update states', () => {
  it('hides the entire panel for an up-to-date agent, including after completion', () => {
    const current = { ...base, current_version: 'v0.1.3', update_available: false }
    expect(render(current)).toBe('')
    expect(render({ ...current, update_state: { state: 'completed', target_version: 'v0.1.3', failure_reason: null } })).toBe('')
  })
  it('shows an available update', () => {
    expect(render(base)).toContain('aria-label="Agent update"')
    expect(render(base)).toContain('Update agent')
    expect(render(base, false)).not.toContain('<button')
  })
  it('shows manual bootstrap without an update button', () => {
    const html = render({ ...base, remote_update_supported: false })
    expect(html).toContain('aria-label="Agent update"')
    expect(html).toContain('updated manually once')
    expect(html).not.toContain('<button')
  })
  it('shows an in-progress update even when no update is currently available', () => {
    const html = render({ ...base, update_available: false, update_state: { state: 'restarting', target_version: 'v0.1.3', failure_reason: null } })
    expect(html).toContain('aria-label="Agent update"')
    expect(html).toContain('Waiting for it to reconnect')
  })
  it('shows a failed update even when no update is currently available', () => {
    const html = render({ ...base, update_available: false, update_state: { state: 'failed', target_version: 'v0.1.3', failure_reason: 'checksum_mismatch' } })
    expect(html).toContain('aria-label="Agent update"')
    expect(html).toContain('checksum mismatch')
  })
})
