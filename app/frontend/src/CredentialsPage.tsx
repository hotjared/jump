import { useEffect, useState } from 'react'

type Credential = { id: string; label: string; kind: string; username: string; domain: string | null }
type Kind = 'windows_password' | 'linux_password' | 'linux_ssh_key'
const labels: Record<Kind, string> = {
  windows_password: 'Windows password', linux_password: 'Linux password', linux_ssh_key: 'SSH private key',
}

export default function CredentialsPage({ admin, mutate }: {
  admin: boolean; mutate: <T>(url: string, method: string, body?: unknown) => Promise<T>
}) {
  const [items, setItems] = useState<Credential[]>([])
  const [kind, setKind] = useState<Kind>('windows_password')
  const [label, setLabel] = useState('')
  const [username, setUsername] = useState('')
  const [domain, setDomain] = useState('')
  const [secret, setSecret] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  useEffect(() => {
    if (!admin) return
    let active = true
    fetch('/api/credentials', { credentials: 'same-origin' })
      .then(r => { if (!r.ok) throw new Error('Could not load credentials'); return r.json() as Promise<Credential[]> })
      .then(data => { if (active) setItems(previous => [...data, ...previous.filter(item => !data.some(saved => saved.id === item.id))]) })
      .catch(e => { if (active) setError(e.message) })
    return () => { active = false }
  }, [admin])

  async function save(event: React.FormEvent) {
    event.preventDefault(); setBusy(true); setError('')
    try {
      const item = await mutate<Credential>('/api/credentials', 'POST', {
        label, kind, username, domain: kind === 'windows_password' ? domain || null : null, secret,
      })
      setItems(previous => [...previous, item]); setSecret(''); setLabel(''); setUsername(''); setDomain('')
    } catch (e) { setError(e instanceof Error ? e.message : 'Could not save credential') }
    finally { setBusy(false) }
  }

  return <><div className="heading"><div><p className="eyebrow">SECURE ACCESS</p><h1>Credentials</h1><p>Save a credential once and use it on compatible devices.</p></div></div>
    {!admin ? <section className="panel">Admin access required.</section> : <div className="settings-grid">
      <section className="panel settings-panel"><h2>Saved credentials</h2>
        {items.map(item => <div className="item manage-item" key={item.id}><span><strong>{item.label}</strong><br />{labels[item.kind as Kind]} · {item.domain ? `${item.domain}\\` : ''}{item.username}</span></div>)}
        {!items.length && <p>No credentials saved yet.</p>}
      </section>
      <section className="panel settings-panel"><h2>Add credential</h2><form className="ssh-credential-form" onSubmit={save}>
        <select aria-label="Credential type" value={kind} onChange={e => { setKind(e.target.value as Kind); setSecret(''); setDomain('') }}>
          {Object.entries(labels).map(([value, name]) => <option key={value} value={value}>{name}</option>)}
        </select>
        <input aria-label="Credential label" placeholder="Label" value={label} onChange={e => setLabel(e.target.value)} required />
        <input aria-label="Credential username" placeholder="Username" value={username} onChange={e => setUsername(e.target.value)} required />
        {kind === 'windows_password' && <input aria-label="Windows domain" placeholder="Domain (optional)" value={domain} onChange={e => setDomain(e.target.value)} />}
        {kind === 'linux_ssh_key' ? <textarea aria-label="SSH private key" placeholder="Paste private key" value={secret} onChange={e => setSecret(e.target.value)} required />
          : <input aria-label="Credential password" type="password" autoComplete="new-password" value={secret} onChange={e => setSecret(e.target.value)} required />}
        <button className="button primary" disabled={busy}>Save credential</button>
      </form>{error && <p className="error" role="alert">{error}</p>}</section>
    </div>}</>
}
