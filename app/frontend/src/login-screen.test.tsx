// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen, waitFor, cleanup } from '@testing-library/react'
import LoginScreen, { type AuthConfig } from './LoginScreen'

const config: AuthConfig = { oidc_enabled: true, local_enabled: false, setup_required: false, csrf: 'preauth' }
afterEach(() => { cleanup(); vi.unstubAllGlobals() })

describe('login modes', () => {
  it('shows only SSO for OIDC', () => {
    render(<LoginScreen config={config} />)
    expect(screen.getByText('Sign in with SSO')).toBeTruthy()
    expect(screen.queryByText('Username')).toBeNull()
  })
  it('shows only credentials for local', () => {
    render(<LoginScreen config={{ ...config, oidc_enabled: false, local_enabled: true }} />)
    expect(screen.getByText('Username')).toBeTruthy()
    expect(screen.getByText('Password')).toBeTruthy()
    expect(screen.queryByText('Sign in with SSO')).toBeNull()
  })
  it('shows both methods for hybrid', () => {
    render(<LoginScreen config={{ ...config, local_enabled: true }} />)
    expect(screen.getByText('Username')).toBeTruthy()
    expect(screen.getByText('Sign in with SSO')).toBeTruthy()
  })
  it('shows setup fields without exposing the token', () => {
    render(<LoginScreen config={{ ...config, local_enabled: true, setup_required: true }} />)
    expect(screen.getByText('Setup token')).toBeTruthy()
    expect(screen.getByText('Display name')).toBeTruthy()
    expect(screen.getByText('Confirm password')).toBeTruthy()
    expect(screen.queryByText('Sign in with SSO')).toBeNull()
  })
  it('shows generic login error from server', async () => {
    const fetcher = vi.fn().mockResolvedValue({ ok: false, json: async () => ({ detail: 'Invalid username or password' }) })
    vi.stubGlobal('fetch', fetcher)
    render(<LoginScreen config={{ ...config, oidc_enabled: false, local_enabled: true }} />)
    fireEvent.change(screen.getByLabelText('Username'), { target: { value: 'admin' } })
    fireEvent.change(screen.getByLabelText('Password'), { target: { value: 'wrong' } })
    fireEvent.click(screen.getByRole('button', { name: 'Sign in' }))
    await waitFor(() => expect(screen.getByRole('alert').textContent).toBe('Invalid username or password'))
    expect(fetcher.mock.calls[0][1].headers['X-CSRF-Token']).toBe('preauth')
  })
})
