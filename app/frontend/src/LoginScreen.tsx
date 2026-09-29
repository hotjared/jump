import { useState, type FormEvent } from 'react'

export type AuthConfig = { oidc_enabled: boolean; local_enabled: boolean; setup_required: boolean; csrf: string }

export default function LoginScreen({ config }: { config: AuthConfig }) {
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [displayName, setDisplayName] = useState('')
  const [confirmation, setConfirmation] = useState('')
  const [token, setToken] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  async function submit(event: FormEvent) {
    event.preventDefault()
    setBusy(true); setError('')
    try {
      const setup = config.setup_required
      const response = await fetch(setup ? '/api/auth/local/setup' : '/api/auth/local/login', {
        method: 'POST', credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': config.csrf },
        body: JSON.stringify(setup ? { token, username, display_name: displayName, password, password_confirmation: confirmation } : { username, password }),
      })
      if (!response.ok) {
        const body = await response.json().catch(() => ({})) as { detail?: string }
        throw new Error(body.detail || `Request failed: ${response.status}`)
      }
      window.location.assign('/')
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : 'Sign in failed')
    } finally { setBusy(false) }
  }

  return <main className="login"><div className="login-card">
    <div className="logo large">J<span>↗</span></div>
    <h1>{config.setup_required ? 'Set up Jump' : 'Your servers, one place.'}</h1>
    {config.setup_required ? <p>Enter the one-time setup token from the Jump container logs to create the first administrator.</p>
      : config.oidc_enabled && !config.local_enabled ? <p>Secure access begins with your identity provider.</p> : null}
    {error && <div className="error" role="alert">{error}</div>}
    {config.local_enabled && <form className="login-form" onSubmit={submit}>
      {config.setup_required && <label>Setup token<input value={token} onChange={e => setToken(e.target.value)} autoComplete="off" required /></label>}
      <label>Username<input value={username} onChange={e => setUsername(e.target.value)} autoComplete="username" required /></label>
      {config.setup_required && <label>Display name<input value={displayName} onChange={e => setDisplayName(e.target.value)} required /></label>}
      <label>Password<input type="password" value={password} onChange={e => setPassword(e.target.value)} autoComplete={config.setup_required ? 'new-password' : 'current-password'} required minLength={config.setup_required ? 12 : undefined} /></label>
      {config.setup_required && <label>Confirm password<input type="password" value={confirmation} onChange={e => setConfirmation(e.target.value)} autoComplete="new-password" required /></label>}
      <button className="button primary" disabled={busy}>{config.setup_required ? 'Create administrator' : 'Sign in'}</button>
    </form>}
    {!config.setup_required && config.oidc_enabled && <>{config.local_enabled && <div className="login-divider">or</div>}<a className="button primary" href="/auth/login">Sign in with SSO</a></>}
  </div></main>
}
