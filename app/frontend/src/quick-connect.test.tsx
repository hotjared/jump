// @vitest-environment jsdom
import { afterEach, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import QuickConnect, { supportedProtocols, type QuickPreference } from './QuickConnect'

vi.mock('./rdp-session', () => ({ RdpSession: class {
  protocol = 'RDP'; state = 'connecting'
  constructor(public id: string, public deviceId: string, public name: string) {}
  disconnect() {}
} }))
vi.mock('./ssh-session', () => ({ SshSession: class {
  protocol = 'SSH'; state = 'connecting'
  constructor(public id: string, public deviceId: string, public name: string) {}
  async ready() { this.state = 'connected' }
  disconnect() {}
} }))

const windows = { id: 'one', hostname: 'WIN', display_name: null, os_family: 'windows', online: true, identity_state: 'active', capabilities: ['rdp_tunnel_v1'] }
const linux = { ...windows, hostname: 'docker', os_family: 'linux', capabilities: ['ssh_terminal_v1'] }
const response = (data: unknown) => ({ ok: true, json: async () => data })
afterEach(() => { cleanup(); vi.unstubAllGlobals() })

it('requires online status and a browser-session capability', () => {
  expect(supportedProtocols(windows)).toEqual(['rdp'])
  expect(supportedProtocols(linux)).toEqual(['ssh'])
  expect(supportedProtocols({ ...windows, online: false })).toEqual([])
  expect(supportedProtocols({ ...windows, capabilities: ['rdp'] })).toEqual([])
  expect(supportedProtocols({ ...linux, capabilities: ['ssh'] })).toEqual([])
})

it('opens first-use settings, saves the credential reference, then starts RDP in the workspace callback', async () => {
  const calls: Array<[string, unknown]> = []
  const onConnected = vi.fn()
  const onPreference = vi.fn()
  vi.stubGlobal('fetch', vi.fn(async () => response([{ id: 'cred', label: 'Admin', username: 'admin', kind: 'windows_password' }])))
  const mutate = async <T,>(url: string, _method: string, body?: unknown): Promise<T> => {
    calls.push([url, body])
    return (url.endsWith('quick-connect-preferences') ? { device_id: 'one', protocol: 'rdp', credential_id: 'cred', preferred: true } : { id: 'session' }) as T
  }
  render(<QuickConnect device={windows} preferences={[]} mutate={mutate} onPreference={onPreference} onConnected={onConnected} onError={vi.fn()} />)
  fireEvent.click(screen.getByRole('button', { name: 'Connect RDP' }))
  await screen.findByRole('combobox', { name: 'Quick Connect credential' })
  fireEvent.change(screen.getByRole('combobox', { name: 'Quick Connect credential' }), { target: { value: 'cred' } })
  fireEvent.click(screen.getByRole('button', { name: 'Connect' }))
  await waitFor(() => expect(onConnected).toHaveBeenCalledTimes(1))
  expect(onPreference).toHaveBeenCalledWith({ device_id: 'one', protocol: 'rdp', credential_id: 'cred', preferred: true })
  expect(calls[0][1]).toEqual({ protocol: 'rdp', credential_id: 'cred', preferred: true })
  expect(calls[1][1]).toMatchObject({ credential_id: 'cred', dpi: 96 })
})

it('uses the saved SSH credential and prevents repeat clicks while the request is pending', async () => {
  let finish!: (value: { id: string }) => void
  const pending = new Promise<{ id: string }>(resolve => { finish = resolve })
  const request = vi.fn(async () => pending)
  const mutate = <T,>(): Promise<T> => request() as Promise<T>
  const onConnected = vi.fn()
  vi.stubGlobal('fetch', vi.fn(async () => response([{ id: 'ssh-cred', label: 'Linux', username: 'user', kind: 'linux_ssh_key' }])))
  const preference: QuickPreference = { device_id: 'one', protocol: 'ssh', credential_id: 'ssh-cred', preferred: true }
  render(<QuickConnect device={linux} preferences={[preference]} mutate={mutate} onPreference={vi.fn()} onConnected={onConnected} onError={vi.fn()} />)
  fireEvent.click(screen.getByRole('button', { name: 'Connect SSH' }))
  await screen.findByRole('button', { name: 'Connecting…' })
  fireEvent.click(screen.getByRole('button', { name: 'Connecting…' }))
  expect(request).toHaveBeenCalledTimes(1)
  finish({ id: 'ssh-session' })
  await waitFor(() => expect(onConnected).toHaveBeenCalledTimes(1))
  expect(onConnected.mock.calls[0][0].protocol).toBe('SSH')
})

it('rejects a stale saved credential and asks for another', async () => {
  const mutate = vi.fn()
  const onError = vi.fn()
  vi.stubGlobal('fetch', vi.fn(async () => response([{ id: 'new', label: 'New', username: 'user', kind: 'windows_password' }])))
  render(<QuickConnect device={windows} preferences={[{ device_id: 'one', protocol: 'rdp', credential_id: 'gone', preferred: true }]} mutate={mutate} onPreference={vi.fn()} onConnected={vi.fn()} onError={onError} />)
  fireEvent.click(screen.getByRole('button', { name: 'Connect RDP' }))
  await screen.findByRole('combobox', { name: 'Quick Connect credential' })
  expect(mutate).not.toHaveBeenCalled()
  expect(onError).toHaveBeenCalledWith('Saved credential is unavailable. Choose another credential.')
})
