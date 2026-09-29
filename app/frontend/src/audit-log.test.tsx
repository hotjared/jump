// @vitest-environment jsdom
import { afterEach, expect, it } from 'vitest'
import { cleanup, fireEvent, render, screen, within } from '@testing-library/react'
import AuditLog from './AuditLog'
import { label, reason, type AuditEvent } from './audit-events'

const event = (id: string, event_type: string, detail: Record<string, unknown> = {}, actor = true): AuditEvent => ({
  id, event_type, detail, created_at: '2026-09-28T23:31:00Z', device_id: 'device-1',
  device: { id: 'device-1', name: 'PROD-DC01' }, actor_user_id: actor ? 'user-1' : null,
  actor: actor ? { name: 'Jared Rodriguez', email: 'jared@example.com' } : null,
  request_id: 'request-1',
})
const entries = [
  event('1', 'rdp_session_failed', { reason: 'guacd_disconnected', session_id: 'abcd1234' }),
  event('2', 'file_download_failed', { filename: 'backup.zip', size: 123,
    reason: 'checksum_mismatch', remote_path: '/private/backup.zip', password: 'password-secret' }, false),
  event('3', 'agent_update_completed', { from_version: 'v0.1.6', target_version: 'v0.1.7' }),
  event('4', 'credential_updated', { label: 'Domain Admin', ciphertext: 'ciphertext-secret', nonce: 'nonce-secret' }),
  event('5', 'unknown_future_event', { token: 'token-secret', arbitrary: 'arbitrary-secret' }),
]
afterEach(cleanup)
const show = (events = entries, admin = true) => render(<AuditLog events={events} admin={admin} />)
const rows = () => screen.getByRole('region', { name: 'Audit activity' }).querySelectorAll('.audit-row')
const choose = (name: string, value: string) => fireEvent.change(screen.getByRole('combobox', { name }), { target: { value } })

it('shows intentional labels, device, actor, System, readable reasons and safe details', () => {
  show()
  expect(label('ssh_session_idle_timeout')).toBe('SSH session idle timeout')
  expect(reason('authentication_failed')).toBe('Authentication failed')
  expect(rows()).toHaveLength(5)
  expect(rows()[0].textContent).toContain('PROD-DC01')
  expect(rows()[0].textContent).toContain('Jared Rodriguez')
  expect(rows()[0].textContent).toContain('RDP protocol service disconnected')
  expect(rows()[1].textContent).toContain('System')
  expect(rows()[1].textContent).toContain('backup.zip · Download checksum did not match')
  expect(rows()[2].textContent).toContain('v0.1.6 → v0.1.7')
  fireEvent.click(screen.getByRole('button', { name: /Expand Download failed/ }))
  const detail = document.querySelector('.audit-details')!
  expect(within(detail as HTMLElement).getByText('123')).toBeTruthy()
  expect(within(detail as HTMLElement).getByText('request-1')).toBeTruthy()
  expect(document.body.textContent).not.toContain('/private/backup.zip')
  for (const secret of ['password-secret', 'ciphertext-secret', 'nonce-secret', 'token-secret', 'arbitrary-secret']) {
    expect(document.body.textContent).not.toContain(secret)
  }
  fireEvent.click(screen.getByRole('button', { name: /Collapse Download failed/ }))
  expect(document.querySelector('.audit-details')).toBeNull()
})

it('filters by category, device, user, outcome and safe displayed text', () => {
  show([...entries, { ...event('6', 'agent_connected', {}, false), device_id: 'device-2',
    device: { id: 'device-2', name: 'docker01' } }])
  choose('Category', 'files'); expect(rows()).toHaveLength(1)
  choose('Category', 'all'); choose('Device', 'device-2'); expect(rows()).toHaveLength(1)
  choose('Device', 'all'); choose('User', 'system'); expect(rows()).toHaveLength(2)
  choose('User', 'all'); choose('Outcome', 'failed'); expect(rows()).toHaveLength(2)
  choose('Outcome', 'all')
  fireEvent.change(screen.getByRole('textbox', { name: 'Search audit activity' }), { target: { value: 'jared@example.com' } })
  expect(rows()).toHaveLength(4)
  fireEvent.change(screen.getByRole('textbox', { name: 'Search audit activity' }), { target: { value: 'backup.zip' } })
  expect(rows()).toHaveLength(1)
  fireEvent.change(screen.getByRole('textbox', { name: 'Search audit activity' }), { target: { value: '/private/backup.zip' } })
  expect(rows()).toHaveLength(0)
  expect(screen.getByText('No audit events match these filters.')).toBeTruthy()
})

it('handles empty, unknown, malformed and non-admin states without raw detail', () => {
  const view = show([])
  expect(screen.getByText('No audit activity yet.')).toBeTruthy()
  view.rerender(<AuditLog events={[entries[4]]} admin />)
  fireEvent.click(screen.getByRole('button', { name: /Expand Unknown future event/ }))
  expect(document.querySelector('.audit-details')?.textContent).toContain('unknown_future_event')
  expect(document.body.textContent).not.toContain('arbitrary-secret')
  view.rerender(<AuditLog events={entries} admin={false} />)
  expect(screen.getByText('Admin access required.')).toBeTruthy()
  expect(screen.queryByRole('region', { name: 'Audit activity' })).toBeNull()
})

it('uses the former device name and organization names, and shows valid SSH fingerprints', () => {
  const fingerprint = `SHA256:${'A'.repeat(42)}/`
  const deleted = { ...event('deleted', 'device_deleted', {
    display_name: 'Former PROD-DC01', hostname: 'prod-dc01', device_uuid: 'secret-uuid',
    remote_path: '/private/secret', password: 'secret-password',
  }), device_id: null, device: null }
  const hostnameOnly = { ...event('hostname-only', 'device_deleted', { hostname: 'legacy01' }), device_id: null, device: null }
  const updates = ['group_updated', 'group_deleted', 'tag_updated', 'tag_deleted'].map((type, i) =>
    event(`org-${i}`, type, { name: `${type} name`, id: 'hidden-id', token: 'secret-token' }))
  const historical = event('historical', 'group_updated', { id: 'old-id' })
  const trusted = event('trust', 'ssh_host_key_trusted', { fingerprint, private_key: 'secret-key' })
  show([deleted, hostnameOnly, ...updates, historical, trusted])
  expect(rows()[0].textContent).toContain('Former PROD-DC01')
  expect(rows()[1].textContent).toContain('legacy01')
  for (const [i, type] of ['group_updated', 'group_deleted', 'tag_updated', 'tag_deleted'].entries()) {
    expect(rows()[i + 2].textContent).toContain(`${type} name`)
  }
  expect(rows()[6].textContent).toContain('Group updated')
  fireEvent.click(screen.getByRole('button', { name: 'Expand Device deleted on Former PROD-DC01' }))
  expect(document.querySelector('.audit-details')?.textContent).toContain('Former PROD-DC01')
  fireEvent.click(screen.getByRole('button', { name: /Expand SSH host key trusted/ }))
  expect(document.querySelector('.audit-details')?.textContent).toContain(fingerprint)
  for (const secret of ['secret-uuid', '/private/secret', 'secret-password', 'hidden-id', 'secret-token', 'secret-key']) {
    expect(document.body.textContent).not.toContain(secret)
  }
})
