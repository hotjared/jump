// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import RemotePanel from './RemotePanel'

const base = { id: 'device', hostname: 'srv01', display_name: null, online: true,
  os_family: 'windows', capabilities: ['rdp', 'rdp_tunnel_v1'] }
const credential = { id: 'cred', label: 'Admin', username: 'Administrator', kind: 'windows_password', domain: 'LAB' }
type Mutate = <T>(url: string, method: string, body?: unknown) => Promise<T>
afterEach(() => { cleanup(); vi.unstubAllGlobals() })

describe('remote desktop eligibility and credentials', () => {
  it('does not offer browser RDP to old Windows agents or Linux', () => {
    const mutate = vi.fn()
    const connect = vi.fn()
    const { rerender } = render(<RemotePanel device={{ ...base, capabilities: ['rdp'] }} admin mutate={mutate} onConnected={connect} onOpenExisting={() => {}} />)
    expect(screen.getByText('Update the Jump agent to enable browser RDP.')).toBeTruthy()
    expect(screen.queryByRole('button', { name: 'Connect' })).toBeNull()
    rerender(<RemotePanel device={{ ...base, os_family: 'linux' }} admin mutate={mutate} onConnected={connect} onOpenExisting={() => {}} />)
    expect(screen.getByText('Browser RDP is available for Windows devices.')).toBeTruthy()
  })

  it('loads a saved credential, disables offline connection, then starts a large workspace session', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => ({ ok: true, json: async () => [credential] })))
    const mutate = vi.fn(async <T,>(_url: string, _method: string, _body?: unknown): Promise<T> => ({ id: 'session-id' }) as T)
    const connected = vi.fn()
    const { rerender } = render(<RemotePanel device={{ ...base, online: false }} admin mutate={mutate as Mutate} onConnected={connected} onOpenExisting={() => {}} />)
    await screen.findByRole('option', { name: 'Admin (LAB\\Administrator)' })
    fireEvent.change(screen.getByRole('combobox', { name: 'Windows credential' }), { target: { value: 'cred' } })
    expect(screen.getByRole('button', { name: 'Connect' }).hasAttribute('disabled')).toBe(true)
    rerender(<RemotePanel device={base} admin mutate={mutate as Mutate} onConnected={connected} onOpenExisting={() => {}} />)
    fireEvent.click(screen.getByRole('button', { name: 'Connect' }))
    await waitFor(() => expect(connected).toHaveBeenCalledOnce())
    expect(mutate.mock.calls[0][0]).toContain('/rdp-sessions')
    expect(mutate.mock.calls[0][2]).toMatchObject({ credential_id: 'cred' })
    expect(connected.mock.calls[0][0].protocol).toBe('RDP')
  })

  it('saves Windows password and optional domain without rendering its secret', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => ({ ok: true, json: async () => [] })))
    const mutate = vi.fn(async <T,>(_url: string, _method: string, _body?: unknown): Promise<T> => credential as T)
    const { container } = render(<RemotePanel device={base} admin mutate={mutate as Mutate} onConnected={() => {}} onOpenExisting={() => {}} />)
    fireEvent.change(screen.getByRole('textbox', { name: 'Credential label' }), { target: { value: 'Admin' } })
    fireEvent.change(screen.getByRole('textbox', { name: 'Windows username' }), { target: { value: 'Administrator' } })
    fireEvent.change(screen.getByRole('textbox', { name: 'Windows domain' }), { target: { value: 'LAB' } })
    fireEvent.change(screen.getByLabelText('Windows password'), { target: { value: 'private-secret' } })
    fireEvent.click(screen.getByRole('button', { name: 'Save credential' }))
    await waitFor(() => expect(mutate).toHaveBeenCalledOnce())
    expect(mutate.mock.calls[0][2]).toMatchObject({ kind: 'windows_password', domain: 'LAB', secret: 'private-secret' })
    await waitFor(() => expect((screen.getByLabelText('Windows password') as HTMLInputElement).value).toBe(''))
    expect(container.textContent).not.toContain('private-secret')
  })
})
