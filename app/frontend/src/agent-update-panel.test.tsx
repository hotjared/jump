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
  it('shows the supported action and the manual bootstrap state', () => {
    expect(render(base)).toContain('Update agent')
    expect(render({ ...base, remote_update_supported: false })).toContain('updated manually once')
    expect(render({ ...base, remote_update_supported: false })).not.toContain('<button')
    expect(render(base, false)).not.toContain('<button')
  })
  it('shows current, progress, failure, and success', () => {
    expect(render({ ...base, current_version: 'v0.1.3', update_available: false })).toContain('Agent is up to date')
    expect(render({ ...base, update_state: { state: 'restarting', target_version: 'v0.1.3', failure_reason: null } })).toContain('Waiting for it to reconnect')
    expect(render({ ...base, update_state: { state: 'failed', target_version: 'v0.1.3', failure_reason: 'checksum_mismatch' } })).toContain('checksum mismatch')
    expect(render({ ...base, current_version: 'v0.1.3', update_available: false, update_state: { state: 'completed', target_version: 'v0.1.3', failure_reason: null } })).toContain('Update successful')
  })
})
