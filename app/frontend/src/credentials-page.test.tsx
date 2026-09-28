// @vitest-environment jsdom
import { afterEach, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import CredentialsPage from './CredentialsPage'
import RemotePanel from './RemotePanel'

const saved = { id: 'one', label: 'Domain Admin', kind: 'windows_password', username: 'jared', domain: 'CONTOSO' }
afterEach(() => { cleanup(); vi.unstubAllGlobals() })

it('creates a user credential, clears the secret, and only renders safe metadata', async () => {
  vi.stubGlobal('fetch', vi.fn(async () => ({ ok: true, json: async () => [] })))
  const mutate = vi.fn(async () => saved)
  const { container } = render(<CredentialsPage admin mutate={mutate as <T>(url: string, method: string, body?: unknown) => Promise<T>} />)
  expect(screen.queryByRole('textbox', { name: 'Credential label' })).toBeNull()
  fireEvent.click(screen.getAllByRole('button', { name: /Add credential/i })[0])
  fireEvent.change(screen.getByRole('textbox', { name: 'Credential label' }), { target: { value: 'Domain Admin' } })
  fireEvent.change(screen.getByRole('textbox', { name: 'Credential username' }), { target: { value: 'jared' } })
  fireEvent.change(screen.getByRole('textbox', { name: 'Windows domain' }), { target: { value: 'CONTOSO' } })
  fireEvent.change(screen.getByLabelText('Credential password'), { target: { value: 'hidden-secret' } })
  fireEvent.click(screen.getByRole('button', { name: 'Save credential' }))
  await waitFor(() => expect(mutate).toHaveBeenCalledWith('/api/credentials', 'POST', expect.objectContaining({ secret: 'hidden-secret' })))
  await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
  await waitFor(() => expect(container.textContent).toContain('CONTOSO\\jared'))
  expect(container.textContent).not.toContain('hidden-secret')
})

it('edits metadata without sending a secret and confirms deletion', async () => {
  vi.stubGlobal('fetch', vi.fn(async () => ({ ok: true, json: async () => [saved] })))
  const mutate = vi.fn(async (_url, method, body) => method === 'PATCH' ? { ...saved, ...body } : { ok: true })
  render(<CredentialsPage admin mutate={mutate as <T>(url: string, method: string, body?: unknown) => Promise<T>} />)
  await screen.findByRole('button', { name: 'Edit Domain Admin' })
  expect(screen.getByText('CONTOSO\\jared')).toBeTruthy()
  fireEvent.click(screen.getByRole('button', { name: 'Edit Domain Admin' }))
  expect((screen.getByLabelText('Replace password') as HTMLInputElement).value).toBe('')
  fireEvent.change(screen.getByRole('textbox', { name: 'Credential label' }), { target: { value: 'New name' } })
  fireEvent.click(screen.getByRole('button', { name: 'Save changes' }))
  await waitFor(() => expect(mutate).toHaveBeenCalledWith('/api/credentials/one', 'PATCH', expect.not.objectContaining({ secret: expect.anything() })))
  await screen.findByRole('button', { name: 'Delete New name' })
  fireEvent.click(screen.getByRole('button', { name: 'Delete New name' }))
  fireEvent.click(screen.getByRole('button', { name: 'Cancel' }))
  expect(screen.getByRole('button', { name: 'Delete New name' })).toBeTruthy()
  fireEvent.click(screen.getByRole('button', { name: 'Delete New name' }))
  fireEvent.click(screen.getByRole('button', { name: 'Delete credential' }))
  await waitFor(() => expect(screen.getByText('No credentials saved yet.')).toBeTruthy())
  expect(mutate).toHaveBeenCalledWith('/api/credentials/one', 'DELETE')
})

it('replaces a key only when entered and denies non-admin forms', async () => {
  vi.stubGlobal('fetch', vi.fn(async () => ({ ok: true, json: async () => [{ ...saved, kind: 'linux_ssh_key', domain: null }] })))
  const mutate = vi.fn(async () => ({ ...saved, kind: 'linux_ssh_key', domain: null }))
  const { rerender } = render(<CredentialsPage admin mutate={mutate as <T>(url: string, method: string, body?: unknown) => Promise<T>} />)
  await screen.findByText('SSH private key')
  fireEvent.click(screen.getByRole('button', { name: 'Edit Domain Admin' }))
  fireEvent.change(screen.getByLabelText('Replace private key'), { target: { value: 'replacement-key' } })
  fireEvent.click(screen.getByRole('button', { name: 'Save changes' }))
  await waitFor(() => expect(mutate).toHaveBeenCalledWith('/api/credentials/one', 'PATCH', expect.objectContaining({ secret: 'replacement-key' })))
  rerender(<CredentialsPage admin={false} mutate={mutate as <T>(url: string, method: string, body?: unknown) => Promise<T>} />)
  expect(screen.getByText('Admin access required.')).toBeTruthy()
  expect(screen.queryByRole('button', { name: /Add credential/i })).toBeNull()
})

it('shows one reusable Windows credential on either compatible device, excluding Linux kinds', async () => {
  const fetcher = vi.fn(async (_url: string) => ({ ok: true, json: async () => [saved, { ...saved, id: 'linux', kind: 'linux_password' }] }))
  vi.stubGlobal('fetch', fetcher)
  const device = { id: 'a', hostname: 'a', display_name: null, online: true, os_family: 'windows', capabilities: ['rdp_tunnel_v1'] }
  const props = { admin: true, mutate: vi.fn(), onConnected: vi.fn(), onOpenExisting: vi.fn() }
  const { rerender } = render(<RemotePanel device={device} {...props} />)
  await screen.findByRole('option', { name: 'Domain Admin (CONTOSO\\jared)' })
  rerender(<RemotePanel key="b" device={{ ...device, id: 'b' }} {...props} />)
  await screen.findByRole('option', { name: 'Domain Admin (CONTOSO\\jared)' })
  expect(screen.queryByRole('option', { name: /linux/ })).toBeNull()
  expect(fetcher).toHaveBeenCalledWith('/api/credentials', expect.anything())
})
