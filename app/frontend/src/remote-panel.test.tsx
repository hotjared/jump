// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import RemotePanel from './RemotePanel'
import ScreenPanel from './ScreenPanel'
import styles from './style.css?raw'

const base = { id: 'device', hostname: 'srv01', display_name: null, online: true,
  os_family: 'windows', capabilities: ['rdp', 'rdp_tunnel_v1'] }
const credential = { id: 'cred', label: 'Admin', username: 'Administrator', kind: 'windows_password', domain: 'LAB' }
type Mutate = <T>(url: string, method: string, body?: unknown) => Promise<T>
afterEach(() => { cleanup(); vi.unstubAllGlobals() })

describe('remote desktop eligibility and credentials', () => {
  it('uses the same section inset for both remote headings, descriptions and controls', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => ({ ok: true, json: async () => [credential] })))
    render(<><ScreenPanel device={base} admin mutate={vi.fn()} onConnected={() => {}} openExisting={() => {}} />
      <RemotePanel device={base} admin mutate={vi.fn()} onConnected={() => {}} onOpenExisting={() => {}} /></>)
    await screen.findByRole('option', { name: 'Admin (LAB\\Administrator)' })
    const control = screen.getByRole('heading', { name: 'Screen Control' }).parentElement!
    const rdp = screen.getByRole('heading', { name: 'Remote Desktop (RDP)' }).parentElement!
    expect(control.className).toBe(rdp.className)
    expect(rdp.contains(screen.getByRole('combobox', { name: 'Windows credential' }))).toBe(true)
    expect(rdp.contains(screen.getByRole('button', { name: 'Connect' }))).toBe(true)
    const style = document.createElement('style')
    style.textContent = styles
    document.head.append(style)
    expect(getComputedStyle(rdp).paddingLeft).toBe('20px')
    expect(getComputedStyle(control).paddingLeft).toBe(getComputedStyle(rdp).paddingLeft)
    style.remove()
  })
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

  it('uses the reusable user credential API and shows no device-specific form', async () => {
    const fetcher = vi.fn(async (_url: string) => ({ ok: true, json: async () => [credential, { ...credential, id: 'ssh', kind: 'linux_password' }] }))
    vi.stubGlobal('fetch', fetcher)
    render(<RemotePanel device={base} admin mutate={vi.fn()} onConnected={() => {}} onOpenExisting={() => {}} />)
    await screen.findByRole('option', { name: 'Admin (LAB\\Administrator)' })
    expect(fetcher.mock.calls[0][0]).toBe('/api/credentials')
    expect(screen.queryByRole('option', { name: /ssh/ })).toBeNull()
    expect(screen.queryByRole('button', { name: 'Save credential' })).toBeNull()
  })
})
