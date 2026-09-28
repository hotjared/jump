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
  fireEvent.change(screen.getByRole('textbox', { name: 'Credential label' }), { target: { value: 'Domain Admin' } })
  fireEvent.change(screen.getByRole('textbox', { name: 'Credential username' }), { target: { value: 'jared' } })
  fireEvent.change(screen.getByRole('textbox', { name: 'Windows domain' }), { target: { value: 'CONTOSO' } })
  fireEvent.change(screen.getByLabelText('Credential password'), { target: { value: 'hidden-secret' } })
  fireEvent.click(screen.getByRole('button', { name: 'Save credential' }))
  await waitFor(() => expect(mutate).toHaveBeenCalledWith('/api/credentials', 'POST', expect.objectContaining({ secret: 'hidden-secret' })))
  await waitFor(() => expect((screen.getByLabelText('Credential password') as HTMLInputElement).value).toBe(''))
  await waitFor(() => expect(container.textContent).toContain('CONTOSO\\jared'))
  expect(container.textContent).not.toContain('hidden-secret')
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
