import { useCallback, useEffect, useRef, useState } from 'react'

type Device = { id: string; os_family: string; online: boolean; identity_state: string; capabilities: string[] }
type Entry = { name: string; type: 'directory' | 'file'; size: number; modified_at: string }
type Transfer = { id: string; filename: string; direction: string; transferred_bytes: number; expected_size: number | null; state: string; failure_reason: string | null }
const human = (size: number) => size < 1024 ? `${size} B` : `${(size / 1024 / 1024).toFixed(1)} MB`
export function childPath(path: string, name: string, windows: boolean) {
  if (windows) return path ? path + (path.endsWith('\\') ? '' : '\\') + name : name
  return (path === '/' ? '' : path) + '/' + name
}
export function parentPath(path: string, windows: boolean) {
  if (windows) {
    if (!path || /^[A-Za-z]:\\$/.test(path)) return ''
    const index = path.lastIndexOf('\\')
    return index <= 2 ? path.slice(0, 3) : path.slice(0, index)
  }
  return path === '/' ? '/' : path.slice(0, path.lastIndexOf('/')) || '/'
}
export default function FilesPanel({ device, csrf }: { device: Device; csrf: string }) {
  const windows = device.os_family === 'windows'
  const [path, setPath] = useState(windows ? '' : '/')
  const [entries, setEntries] = useState<Entry[]>([])
  const [more, setMore] = useState(false)
  const [selected, setSelected] = useState<Entry | null>(null)
  const [transfers, setTransfers] = useState<Transfer[]>([])
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState('')
  const [uploadProgress, setUploadProgress] = useState<number | null>(null)
  const picker = useRef<HTMLInputElement>(null)
  const upload = useRef<XMLHttpRequest | null>(null)
  const supported = device.capabilities.includes('file_transfer_v1')
  const base = `/api/devices/${device.id}`
  const refresh = useCallback(async (dir: string, offset = 0) => {
    setLoading(true); setError(''); setSelected(null)
    try {
      const response = await fetch(`${base}/files?path=${encodeURIComponent(dir)}&offset=${offset}`, { credentials: 'same-origin' })
      if (!response.ok) throw new Error((await response.json()).detail || 'Could not list files')
      const listing = await response.json() as { entries: Entry[]; more: boolean }
      setEntries(previous => offset ? [...previous, ...listing.entries] : listing.entries); setMore(listing.more); setPath(dir)
    } catch (e) { setError(e instanceof Error ? e.message : 'Could not list files') }
    finally { setLoading(false) }
  }, [base])
  useEffect(() => { if (device.online && supported) void refresh(windows ? '' : '/') }, [device.id, device.online, supported, windows, refresh])
  useEffect(() => {
    if (!device.online || !supported) return
    const poll = async () => {
      try {
        const response = await fetch(`${base}/file-transfers`, { credentials: 'same-origin' })
        if (response.ok) setTransfers(await response.json() as Transfer[])
      } catch { /* transient polling failure */ }
    }
    void poll()
    const timer = setInterval(() => void poll(), 1000)
    return () => clearInterval(timer)
  }, [base, device.online, supported])
  useEffect(() => () => upload.current?.abort(), [])

  function startUpload(file: File) {
    if (!path) return
    const existing = entries.find(entry => entry.name.toLowerCase() === file.name.toLowerCase())
    if (existing?.type === 'directory') { setError('A directory already has that name.'); return }
    const overwrite = Boolean(existing)
    if (overwrite && !window.confirm(`Overwrite ${file.name} on this device?`)) return
    const request = new XMLHttpRequest()
    upload.current = request
    const query = new URLSearchParams({ path, filename: file.name, size: String(file.size), overwrite: String(overwrite) })
    request.open('POST', `${base}/files/upload?${query}`)
    request.withCredentials = true
    request.setRequestHeader('Content-Type', 'application/octet-stream')
    request.setRequestHeader('X-CSRF-Token', csrf)
    request.upload.onprogress = event => { if (event.lengthComputable) setUploadProgress(event.loaded / event.total) }
    request.onload = () => {
      setUploadProgress(null); upload.current = null
      if (request.status >= 200 && request.status < 300) void refresh(path)
      else { try { setError((JSON.parse(request.responseText) as { detail: string }).detail) } catch { setError('Upload failed') } }
    }
    request.onerror = () => { setUploadProgress(null); upload.current = null; setError('Upload failed') }
    request.onabort = () => { setUploadProgress(null); upload.current = null }
    setUploadProgress(0)
    request.send(file)
  }
  async function cancel(transfer: Transfer) {
    try {
      const response = await fetch(`/api/file-transfers/${transfer.id}/cancel`, { method: 'POST', credentials: 'same-origin', headers: { 'Origin': location.origin, 'X-CSRF-Token': csrf } })
      if (!response.ok) throw new Error('Could not cancel transfer')
      if (transfer.direction === 'upload') upload.current?.abort()
      setTransfers(previous => previous.map(item => item.id === transfer.id ? { ...item, state: 'cancelled' } : item))
    } catch { setError('Could not cancel transfer') }
  }
  if (!device.online) return <div className="placeholder compact">Device is offline.</div>
  if (!supported) return <div className="placeholder compact">Update the Jump agent to enable file transfer.</div>
  return <div className="files-panel details">
    <h3>FILES</h3><div className="files-path">{path || 'This PC'}</div>
    <button className="text-button" disabled={windows ? path === '' : path === '/'} onClick={() => void refresh(parentPath(path, windows))}>↑ Parent</button>
    {loading ? <p>Loading…</p> : <div className="files-list" aria-label="Remote files">
      {entries.map(entry => <button key={entry.name} type="button" className={selected?.name === entry.name ? 'file-entry selected' : 'file-entry'}
        onClick={() => entry.type === 'directory' ? void refresh(childPath(path, entry.name, windows)) : setSelected(entry)}>
        <span>{entry.type === 'directory' ? '📁' : '📄'} {entry.name}</span><small>{entry.type === 'file' ? human(entry.size) : ''}</small>
      </button>)}
      {!entries.length && <p className="muted">This directory is empty.</p>}
    </div>}
    {more && <button className="text-button" disabled={loading} onClick={() => void refresh(path, entries.length)}>Load more</button>}
    {selected && <div className="files-selection"><strong>{selected.name}</strong><a className="button" href={`${base}/files/download?path=${encodeURIComponent(childPath(path, selected.name, windows))}`}>Download</a></div>}
    <input ref={picker} hidden type="file" aria-label="Choose file to upload" onChange={event => { const file = event.target.files?.[0]; if (file) startUpload(file); event.target.value = '' }} />
    <button className="button" disabled={!path || uploadProgress !== null} onClick={() => picker.current?.click()}>Upload file</button>
    {uploadProgress !== null && <p role="status">Sending upload: {Math.round(uploadProgress * 100)}%</p>}
    {error && <p className="session-error" role="alert">{error}</p>}
    <h3>Transfers</h3>
    {transfers.map(transfer => <div className="file-transfer" key={transfer.id}>
      <strong>{transfer.filename}</strong><span>{transfer.direction} · {transfer.state} · {human(transfer.transferred_bytes)}{transfer.expected_size !== null && ` / ${human(transfer.expected_size)}`}</span>
      {transfer.expected_size !== null && transfer.expected_size > 0 && <progress value={transfer.transferred_bytes} max={transfer.expected_size} />}
      {transfer.failure_reason && <small>{transfer.failure_reason.replaceAll('_', ' ')}</small>}
      {(transfer.state === 'active' || transfer.state === 'pending') && <button className="text-button" onClick={() => void cancel(transfer)}>Cancel</button>}
    </div>)}
  </div>
}
