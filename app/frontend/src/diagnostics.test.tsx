// @vitest-environment jsdom
import { afterEach, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import Diagnostics from './Diagnostics'

afterEach(() => { cleanup(); vi.unstubAllGlobals() })
const result = {
  components: { api: { status: 'healthy', detail: 'Responding' }, database: { status: 'healthy', detail: 'Connected' },
    broker: { status: 'unavailable', detail: 'Not reachable' }, guacd: { status: 'healthy', detail: 'TCP reachable' } },
  runtime: { devices_online: 6, active_sessions: 2, active_file_transfers: 0, active_agent_updates: 1 },
  broker_runtime: null,
  recent_failures: [{ id: 'f1', event_type: 'rdp_session_failed', device_name: 'PROD-DC01', created_at: '2026-09-29T12:00:00Z',
    detail: { reason: 'guacd_disconnected', session_id: '12345678-1234-1234-1234-123456789012', remote_path: '/private/secret', password: 'do-not-show' }, request_id: null }],
}

it('requires admin without fetching', () => {
  const fetcher = vi.fn()
  vi.stubGlobal('fetch', fetcher)
  render(<Diagnostics admin={false} />)
  expect(screen.getByText('Admin access required.')).toBeTruthy()
  expect(fetcher).not.toHaveBeenCalled()
})

it('shows health, counts, safe failures and refreshes on demand', async () => {
  const fetcher = vi.fn().mockImplementation((url: string) => Promise.resolve({ ok: true, json: async () => url.endsWith('/sessions') ? [] : result }))
  vi.stubGlobal('fetch', fetcher)
  render(<Diagnostics admin />)
  expect(await screen.findByText('PROD-DC01')).toBeTruthy()
  expect(screen.getByText('Unavailable')).toBeTruthy()
  expect(screen.getAllByText('RDP protocol service disconnected')).toHaveLength(2)
  expect(screen.getByText('6')).toBeTruthy()
  expect(screen.getByText('2')).toBeTruthy()
  expect(screen.queryByText('/private/secret')).toBeNull()
  expect(screen.queryByText('do-not-show')).toBeNull()
  fireEvent.click(screen.getByRole('button', { name: 'Refresh' }))
  await waitFor(() => expect(fetcher).toHaveBeenCalledTimes(4))
  expect(fetcher).toHaveBeenCalledWith('/api/diagnostics', expect.objectContaining({ credentials: 'same-origin' }))
  expect(fetcher).toHaveBeenCalledWith('/api/diagnostics/sessions', expect.objectContaining({ credentials: 'same-origin' }))
})

it('shows SSH and RDP traces with IDs only in expanded details', async () => {
  const session = (protocol: 'ssh' | 'rdp', state: 'closed' | 'failed', name: string) => ({
    id: `${protocol}-session-id`, protocol, state, device: { id: 'device-id', name },
    created_at: '2026-09-29T12:00:00Z', attached_at: '2026-09-29T12:00:01Z', closed_at: '2026-09-29T12:00:03Z',
    request_id: `${protocol}-request-id`, failure_reason: state === 'failed' ? 'authentication_failed' : null,
    stages: ['session_created', 'browser_attached', 'broker_connected', protocol === 'ssh' ? 'host_key_verified' : 'protocol_ready',
      state === 'closed' ? 'session_closed' : 'session_failed'].map((stage, index) => ({ stage, created_at: `2026-09-29T12:00:0${index}Z` })),
    password: 'do-not-render', connection_id: 'private-connection',
  })
  const fetcher = vi.fn().mockImplementation((url: string) => Promise.resolve({ ok: true, json: async () =>
    url.endsWith('/sessions') ? [session('ssh', 'closed', 'docker01'), session('rdp', 'failed', 'Deleted device')] : result }))
  vi.stubGlobal('fetch', fetcher)
  render(<Diagnostics admin />)
  const section = await screen.findByRole('region', { name: 'Recent remote sessions' })
  expect(section.textContent).toContain('docker01')
  expect(section.textContent).toContain('Deleted device')
  expect(section.textContent).toContain('SSH')
  expect(section.textContent).toContain('RDP')
  expect(section.querySelectorAll('summary')[0].textContent).not.toContain('ssh-session-id')
  expect(section.querySelectorAll('summary')[1].textContent).not.toContain('rdp-request-id')
  expect(section.textContent).toContain('Host key verified')
  expect(section.textContent).toContain('Protocol ready')
  expect(section.textContent).toContain('Authentication failed')
  expect(section.textContent).not.toContain('do-not-render')
  expect(section.textContent).not.toContain('private-connection')
})
