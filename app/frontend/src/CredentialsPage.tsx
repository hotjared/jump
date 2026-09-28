import { useEffect, useState } from 'react'

type Kind = 'windows_password' | 'linux_password' | 'linux_ssh_key'
type Credential = { id: string; label: string; kind: Kind; username: string; domain: string | null }
type Mode = 'add' | 'edit' | 'delete' | null
const labels: Record<Kind, string> = {
  windows_password: 'Windows password', linux_password: 'Linux password', linux_ssh_key: 'SSH private key',
}

export default function CredentialsPage({ admin, mutate }: {
  admin: boolean; mutate: <T>(url: string, method: string, body?: unknown) => Promise<T>
}) {
  const [items, setItems] = useState<Credential[]>([])
  const [mode, setMode] = useState<Mode>(null)
  const [selected, setSelected] = useState<Credential | null>(null)
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
      .catch(e => { if (active) setError(e instanceof Error ? e.message : 'Could not load credentials') })
    return () => { active = false }
  }, [admin])

  function close() {
    if (busy) return
    setMode(null); setSelected(null); setSecret(''); setLabel(''); setUsername(''); setDomain(''); setError('')
  }
  function open(nextMode: Exclude<Mode, null>, item?: Credential) {
    setMode(nextMode); setSelected(item || null); setKind(item?.kind || 'windows_password')
    setLabel(item?.label || ''); setUsername(item?.username || ''); setDomain(item?.domain || '')
    setSecret(''); setError('')
  }

  async function save(event: React.FormEvent) {
    event.preventDefault(); setBusy(true); setError('')
    try {
      const body = {
        label, username, domain: kind === 'windows_password' ? domain || null : null,
        ...(mode === 'add' ? { kind, secret } : secret ? { secret } : {}),
      }
      const item = await mutate<Credential>(mode === 'edit' ? `/api/credentials/${selected!.id}` : '/api/credentials', mode === 'edit' ? 'PATCH' : 'POST', body)
      setItems(previous => mode === 'edit' ? previous.map(saved => saved.id === item.id ? item : saved) : [...previous, item])
      setBusy(false); close()
    } catch (e) { setError(e instanceof Error ? e.message : 'Could not save credential'); setBusy(false) }
  }

  async function remove() {
    if (!selected) return
    setBusy(true); setError('')
    try {
      await mutate(`/api/credentials/${selected.id}`, 'DELETE')
      setItems(previous => previous.filter(item => item.id !== selected.id))
      setBusy(false); close()
    } catch (e) { setError(e instanceof Error ? e.message : 'Could not delete credential'); setBusy(false) }
  }

  return <><div className="heading"><div><p className="eyebrow">SECURE ACCESS</p><h1>Credentials</h1><p>Save a credential once and use it on compatible devices.</p></div>
    {admin && <button className="button primary" onClick={() => open('add')}>+ Add credential</button>}</div>
    {!admin ? <section className="panel">Admin access required.</section> : <>
      {error && !mode && <p className="error" role="alert">{error}</p>}
      <section className="panel credential-inventory" aria-label="Saved credentials">
        <h2 className="panel-title">Saved credentials</h2>
        {items.length ? <><div className="credential-header" aria-hidden="true"><span>Name</span><span>Type</span><span>Username</span><span>Actions</span></div>
          {items.map(item => <div className="credential-row" key={item.id}>
            <strong className="credential-name">{item.label}</strong>
            <span className="credential-type">{labels[item.kind]}</span>
            <span className="credential-username">{item.domain ? `${item.domain}\\${item.username}` : item.username}</span>
            <div className="credential-actions"><button className="text-button" onClick={() => open('edit', item)} aria-label={`Edit ${item.label}`}>Edit</button><button className="text-button destructive" onClick={() => open('delete', item)} aria-label={`Delete ${item.label}`}>Delete</button></div>
          </div>)}</> : <div className="empty"><strong>No credentials saved yet.</strong><p>Save a credential once and reuse it across compatible devices.</p><button className="button" onClick={() => open('add')}>Add credential</button></div>}
      </section>
    </>}
    {mode && <div className="overlay credential-overlay" onMouseDown={e => { if (e.target === e.currentTarget) close() }}>
      <section className="modal credential-modal" role="dialog" aria-modal="true" aria-labelledby="credential-dialog-title">
        <button className="close" aria-label="Close dialog" onClick={close} disabled={busy}>×</button>
        <h2 id="credential-dialog-title">{mode === 'delete' ? 'Delete credential' : mode === 'edit' ? 'Edit credential' : 'Add credential'}</h2>
        {mode === 'delete' ? <><p>Delete “{selected?.label}”?</p><p>Any Quick Connect selections using this credential will be cleared. Existing session and audit history will remain.</p>
          {error && <p className="error" role="alert">{error}</p>}
          <div className="credential-modal-actions"><button className="button" onClick={close} disabled={busy}>Cancel</button><button className="button credential-delete" onClick={remove} disabled={busy}>Delete credential</button></div></>
          : <form className="credential-form" onSubmit={save}>
            {mode === 'add' ? <label>Type<select aria-label="Credential type" value={kind} onChange={e => { setKind(e.target.value as Kind); setSecret(''); setDomain('') }}>
              {Object.entries(labels).map(([value, name]) => <option key={value} value={value}>{name}</option>)}
            </select></label> : <p>Type: {labels[kind]}</p>}
            <label>Label<input aria-label="Credential label" value={label} onChange={e => setLabel(e.target.value)} required /></label>
            <label>Username<input aria-label="Credential username" value={username} onChange={e => setUsername(e.target.value)} required /></label>
            {kind === 'windows_password' && <label>Domain (optional)<input aria-label="Windows domain" value={domain} onChange={e => setDomain(e.target.value)} /></label>}
            <label>{mode === 'edit' ? kind === 'linux_ssh_key' ? 'Replace private key' : 'Replace password' : kind === 'linux_ssh_key' ? 'Private key' : 'Password'}
              {kind === 'linux_ssh_key' ? <textarea aria-label={mode === 'edit' ? 'Replace private key' : 'SSH private key'} value={secret} onChange={e => setSecret(e.target.value)} required={mode === 'add'} />
                : <input aria-label={mode === 'edit' ? 'Replace password' : 'Credential password'} type="password" autoComplete="new-password" value={secret} onChange={e => setSecret(e.target.value)} required={mode === 'add'} />}</label>
            {mode === 'edit' && <p className="credential-hint">Leave blank to keep the existing secret.</p>}
            {error && <p className="error" role="alert">{error}</p>}
            <div className="credential-modal-actions"><button type="button" className="button" onClick={close} disabled={busy}>Cancel</button><button className="button primary" disabled={busy}>{mode === 'edit' ? 'Save changes' : 'Save credential'}</button></div>
          </form>}
      </section>
    </div>}
  </>
}
