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
  const fetcher = vi.fn().mockResolvedValue({ ok: true, json: async () => result })
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
  await waitFor(() => expect(fetcher).toHaveBeenCalledTimes(2))
  expect(fetcher).toHaveBeenCalledWith('/api/diagnostics', expect.objectContaining({ credentials: 'same-origin' }))
})
