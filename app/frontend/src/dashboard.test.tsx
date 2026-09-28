// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen, within } from '@testing-library/react'
import Dashboard, { type DashboardDevice, type DashboardEvent } from './Dashboard'
import type { WorkspaceSession } from './SessionWorkspace'

const base = (id: string): DashboardDevice => ({
  id, hostname: id, display_name: null, online: true, identity_state: 'active',
  agent_update: { update_available: false, current_version: '0.1.4', latest_version: '0.1.5', update_state: null },
})
const event = (id: string, event_type: string, created_at = new Date().toISOString(), detail?: Record<string, unknown>): DashboardEvent =>
  ({ id, event_type, created_at, device_id: 'docker01', detail })
const session = (id: string, protocol: 'SSH' | 'RDP') => ({
  id, name: id, protocol, state: 'connected', subscribe: () => () => {},
}) as unknown as WorkspaceSession
const show = (devices: DashboardDevice[] = [], sessions: WorkspaceSession[] = [], events: DashboardEvent[] = [], open = vi.fn()) =>
  render(<Dashboard devices={devices} sessions={sessions} events={events} open={open} />)
afterEach(cleanup)

describe('operational dashboard', () => {
  it('counts workspace tabs, online devices, eligible updates and rendered issues', () => {
    show([
      { ...base('docker01'), agent_update: { ...base('docker01').agent_update, update_available: true } },
      { ...base('offline'), online: false },
      { ...base('revoked'), identity_state: 'revoked', agent_update: { ...base('revoked').agent_update, update_available: true } },
    ], [session('ssh-1', 'SSH'), session('rdp-1', 'RDP')])
    const cards = document.querySelector('.dashboard-stats')!
    expect(cards.textContent).toContain('Active Sessions2')
    expect(cards.textContent).toContain('Devices Online2 / 3')
    expect(cards.textContent).toContain('Agent Updates1')
    expect(cards.textContent).toContain('Issues2')
    expect(within(screen.getByRole('region', { name: 'Needs Attention' })).getAllByText(/Agent update available|Device revoked/)).toHaveLength(2)
  })

  it('shows failed updates and safe recent failures, without dumping audit detail', () => {
    const failed = { ...base('docker01'), agent_update: { ...base('docker01').agent_update,
      update_state: { state: 'failed', failure_reason: 'reconnect_timeout', completed_at: new Date().toISOString() } } }
    show([failed], [], [event('f', 'file_download_failed', undefined,
      { filename: 'report.pdf', reason: 'transfer_failed', remote_path: '/secret/path', token: 'secret-token' })])
    const issues = screen.getByRole('region', { name: 'Needs Attention' })
    expect(within(issues).getByText('Agent update failed')).toBeTruthy()
    expect(within(issues).getByText('reconnect timeout')).toBeTruthy()
    expect(within(issues).getByText('report.pdf · transfer failed')).toBeTruthy()
    expect(issues.textContent).not.toContain('/secret/path')
    expect(document.body.textContent).not.toContain('secret-token')
  })

  it('shows a failed outdated agent as one issue instead of duplicating update available', () => {
    const failed = { ...base('docker01'), agent_update: { ...base('docker01').agent_update,
      update_available: true,
      update_state: { state: 'failed', failure_reason: 'reconnect_timeout', completed_at: new Date().toISOString() } } }
    show([failed])
    const issues = screen.getByRole('region', { name: 'Needs Attention' })
    expect(within(issues).getByText('Agent update failed')).toBeTruthy()
    expect(within(issues).queryByText('Agent update available')).toBeNull()
    expect(document.querySelector('.dashboard-stats')?.textContent).toContain('Agent Updates1')
    expect(document.querySelector('.dashboard-stats')?.textContent).toContain('Issues1')
  })

  it('treats ordinary offline devices as normal and shows empty states', () => {
    show([{ ...base('offline'), online: false }])
    expect(screen.getByText('Nothing needs attention.')).toBeTruthy()
    expect(screen.getByText('No active sessions.')).toBeTruthy()
  })

  it('opens an existing SSH or RDP session by ID', () => {
    const open = vi.fn()
    show([], [session('shell', 'SSH'), session('desktop', 'RDP')], [], open)
    fireEvent.click(screen.getByRole('button', { name: 'Open shell SSH session' }))
    fireEvent.click(screen.getByRole('button', { name: 'Open desktop RDP session' }))
    expect(open.mock.calls).toEqual([['shell'], ['desktop']])
  })

  it('orders known activity newest first, labels it and omits unknown events', () => {
    show([base('docker01')], [], [
      event('older', 'ssh_session_started', '2026-01-01T00:00:00Z', { credential_id: 'secret' }),
      event('unknown', 'user_login', '2026-01-03T00:00:00Z', { token: 'secret' }),
      event('newer', 'file_upload_completed', '2026-01-02T00:00:00Z', { filename: 'config.txt', remote_path: '/hidden' }),
    ])
    const activity = screen.getByRole('region', { name: 'Recent Activity' })
    const rows = activity.querySelectorAll('.dashboard-row')
    expect(rows).toHaveLength(2)
    expect(rows[0].textContent).toContain('Uploaded config.txt on docker01')
    expect(rows[1].textContent).toContain('SSH session started on docker01')
    expect(activity.textContent).not.toContain('/hidden')
    expect(activity.textContent).not.toContain('secret')
  })
})
