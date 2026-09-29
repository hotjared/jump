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
  const fetcher = vi.fn().mockImplementation((url: string) => Promise.resolve({ ok: true, json: async () => url.endsWith('/sessions') || url.endsWith('/file-transfers') ? [] : result }))
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
  await waitFor(() => expect(fetcher).toHaveBeenCalledTimes(6))
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
    url.endsWith('/file-transfers') ? [] : url.endsWith('/sessions') ? [session('ssh', 'closed', 'docker01'), session('rdp', 'failed', 'Deleted device')] : result }))
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

it('keeps core diagnostics visible when session traces fail and retries both on Refresh', async () => {
  let tracesAvailable = false
  const trace = { id: 'session-1', protocol: 'ssh', state: 'closed', device: { id: null, name: 'Former docker01' },
    created_at: '2026-09-29T12:00:00Z', attached_at: null, closed_at: '2026-09-29T12:00:03Z',
    failure_reason: null, request_id: null, stages: [{ stage: 'session_created', created_at: '2026-09-29T12:00:00Z' }] }
  const fetcher = vi.fn().mockImplementation((url: string) => Promise.resolve(url.endsWith('/file-transfers') ? { ok: true, json: async () => [] } : url.endsWith('/sessions')
    ? { ok: tracesAvailable, json: async () => [trace] }
    : { ok: true, json: async () => result }))
  vi.stubGlobal('fetch', fetcher)
  render(<Diagnostics admin />)
  expect(await screen.findByText('Jump API')).toBeTruthy()
  expect(await screen.findByText('Session diagnostics unavailable.')).toBeTruthy()
  expect(screen.getByText('PROD-DC01')).toBeTruthy()
  expect(screen.queryByText('Could not refresh diagnostics.')).toBeNull()
  tracesAvailable = true
  fireEvent.click(screen.getByRole('button', { name: 'Refresh' }))
  expect(await screen.findByText('Former docker01')).toBeTruthy()
  expect(screen.queryByText('Session diagnostics unavailable.')).toBeNull()
  expect(fetcher).toHaveBeenCalledTimes(6)
  expect(fetcher.mock.calls.filter(([url]) => url === '/api/diagnostics')).toHaveLength(2)
  expect(fetcher.mock.calls.filter(([url]) => url === '/api/diagnostics/sessions')).toHaveLength(2)
  expect(fetcher.mock.calls.filter(([url]) => url === '/api/diagnostics/file-transfers')).toHaveLength(2)
})

it('keeps the last core result and shows the page error when core refresh fails', async () => {
  let coreAvailable = true
  const fetcher = vi.fn().mockImplementation((url: string) => Promise.resolve(url.endsWith('/sessions') || url.endsWith('/file-transfers')
    ? { ok: true, json: async () => [] }
    : { ok: coreAvailable, json: async () => result }))
  vi.stubGlobal('fetch', fetcher)
  render(<Diagnostics admin />)
  expect(await screen.findByText('Jump API')).toBeTruthy()
  coreAvailable = false
  fireEvent.click(screen.getByRole('button', { name: 'Refresh' }))
  expect(await screen.findByRole('alert')).toHaveProperty('textContent', 'Could not refresh diagnostics. Showing the last check.')
  expect(screen.getByText('Jump API')).toBeTruthy()
  expect(screen.queryByText('Session diagnostics unavailable.')).toBeNull()
  await waitFor(() => expect(fetcher).toHaveBeenCalledTimes(6))
})

it('renders transfer traces and safe identities, and retries an independent failure', async () => {
  let available = true
  const transfer = (state: 'completed' | 'failed' | 'cancelled', filename: string) => ({
    id: `${state}-transfer-id`, direction: state === 'completed' ? 'download' : 'upload', state, filename,
    device: { id: null, name: 'Former PROD-DC01' }, expected_size: 12, transferred_bytes: 12,
    failure_reason: state === 'failed' ? 'permission_denied' : state === 'cancelled' ? 'transfer_cancelled' : null,
    created_at: '2026-09-29T12:00:00Z', completed_at: '2026-09-29T12:00:08Z',
    stages: ['transfer_created', state === 'completed' ? 'checksum_verified' : `transfer_${state}`]
      .map((stage, index) => ({ stage, created_at: `2026-09-29T12:00:0${index}Z` })),
    remote_path: '/secret/file', connection_id: 'private-connection', content: 'secret bytes',
  })
  const fetcher = vi.fn().mockImplementation((url: string) => Promise.resolve(url.endsWith('/file-transfers')
    ? { ok: available, json: async () => [transfer('completed', 'backup.zip'), transfer('failed', 'config.txt'),
      transfer('cancelled', '/secret/leaked.txt'), { ...transfer('completed', 'old.txt'), id: 'old', stages: [] }] }
    : { ok: true, json: async () => url.endsWith('/sessions') ? [] : result }))
  vi.stubGlobal('fetch', fetcher)
  render(<Diagnostics admin />)
  const section = await screen.findByRole('region', { name: 'Recent file transfers' })
  await waitFor(() => expect(section.textContent).toContain('backup.zip'))
  expect(section.textContent).toContain('Download')
  expect(section.textContent).toContain('Upload')
  expect(section.textContent).toContain('Former PROD-DC01')
  expect(section.textContent).toContain('SHA-256 verified')
  expect(section.textContent).toContain('Permission denied')
  expect(section.textContent).toContain('Transfer cancelled')
  expect(section.textContent).toContain('No detailed trace available for this older transfer.')
  expect(section.querySelector('summary')?.textContent).not.toContain('completed-transfer-id')
  expect(section.textContent).not.toContain('/secret')
  expect(section.textContent).not.toContain('private-connection')
  expect(section.textContent).not.toContain('secret bytes')
  available = false
  fireEvent.click(screen.getByRole('button', { name: 'Refresh' }))
  expect(await screen.findByText('File transfer diagnostics unavailable.')).toBeTruthy()
  expect(section.textContent).toContain('backup.zip')
  expect(screen.getByText('Jump API')).toBeTruthy()
  expect(screen.getByRole('region', { name: 'Recent remote sessions' })).toBeTruthy()
  available = true
  fireEvent.click(screen.getByRole('button', { name: 'Refresh' }))
  await waitFor(() => expect(screen.queryByText('File transfer diagnostics unavailable.')).toBeNull())
  expect(fetcher.mock.calls.filter(([url]) => url === '/api/diagnostics/file-transfers')).toHaveLength(3)
})
