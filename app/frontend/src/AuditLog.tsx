import { useState } from 'react'
import { category, safeDetails, summary, type AuditEvent } from './audit-events'
import './audit-log.css'

const categories = [
  ['all', 'All activity'], ['sessions', 'Sessions'], ['files', 'Files'],
  ['agents', 'Agents & devices'], ['credentials', 'Credentials'],
  ['security', 'Security & access'], ['organization', 'Organization'], ['other', 'Other'],
]
const actorName = (event: AuditEvent) => event.actor?.name || (event.actor_user_id ? 'Deleted user' : 'System')
const deviceName = (event: AuditEvent) => event.device?.name || (event.device_id || event.event_type === 'device_deleted' ? 'Deleted device' : '—')

export default function AuditLog({ events, admin }: { events: AuditEvent[]; admin: boolean }) {
  const [query, setQuery] = useState('')
  const [categoryFilter, setCategory] = useState('all')
  const [device, setDevice] = useState('all')
  const [actor, setActor] = useState('all')
  const [outcome, setOutcome] = useState('all')
  const [expanded, setExpanded] = useState<string | null>(null)
  const devices = [...new Map(events.filter(e => e.device_id).map(e => [e.device_id!, deviceName(e)])).entries()]
  const actors = [...new Map(events.map(e => [e.actor_user_id || 'system', actorName(e)])).entries()]
  const filtered = events.filter(e => {
    const info = summary(e)
    const q = query.trim().toLowerCase()
    return (categoryFilter === 'all' || category(e.event_type) === categoryFilter) &&
      (device === 'all' || e.device_id === device) &&
      (actor === 'all' || (e.actor_user_id || 'system') === actor) &&
      (outcome === 'all' || (e.event_type.endsWith('_failed') ? 'failed' : 'other') === outcome) &&
      (!q || [info.title, info.subtitle, deviceName(e), actorName(e), e.actor?.email,
        ...safeDetails(e).filter(d => ['filename', 'reason'].includes(d.key)).map(d => d.value)]
        .some(value => value?.toLowerCase().includes(q)))
  })
  return <div className="audit-log">
    <div className="heading"><div><p className="eyebrow">SECURITY</p><h1>Audit Log</h1><p>Recent activity in your Jump deployment.</p></div></div>
    {!admin ? <section className="panel"><div className="empty">Admin access required.</div></section> : <>
      <div className="audit-filters">
        <input aria-label="Search audit activity" placeholder="Search activity…" value={query} onChange={e => setQuery(e.target.value)} />
        <select aria-label="Category" value={categoryFilter} onChange={e => setCategory(e.target.value)}>{categories.map(([value, name]) => <option value={value} key={value}>{name}</option>)}</select>
        <select aria-label="Device" value={device} onChange={e => setDevice(e.target.value)}><option value="all">All devices</option>{devices.map(([id, name]) => <option key={id} value={id}>{name}</option>)}</select>
        <select aria-label="User" value={actor} onChange={e => setActor(e.target.value)}><option value="all">All users</option>{actors.map(([id, name]) => <option key={id} value={id}>{name}</option>)}</select>
        <select aria-label="Outcome" value={outcome} onChange={e => setOutcome(e.target.value)}><option value="all">All outcomes</option><option value="other">Successful / informational</option><option value="failed">Failed / attention</option></select>
      </div>
      <section className="panel audit-panel" aria-label="Audit activity">
        <div className="audit-heading"><span>EVENT</span><span>DEVICE</span><span>USER</span><span>TIME</span><span /></div>
        {filtered.map(e => {
          const info = summary(e)
          const details = safeDetails(e)
          const open = expanded === e.id
          return <div className="audit-entry" key={e.id}>
            <button className="audit-row" aria-expanded={open} aria-label={`${open ? 'Collapse' : 'Expand'} ${info.title} on ${deviceName(e)}`} onClick={() => setExpanded(open ? null : e.id)}>
              <span className="audit-event"><strong>{info.title}</strong>{info.subtitle && <small>{info.subtitle}</small>}</span>
              <span className="audit-device">{deviceName(e)}</span>
              <span className="audit-user">{actorName(e)}</span>
              <time className="audit-time" dateTime={e.created_at}>{new Date(e.created_at).toLocaleString()}</time>
              <span className="audit-chevron" aria-hidden="true">{open ? '⌄' : '›'}</span>
            </button>
            {open && <div className="audit-details">
              {[['Event type', e.event_type], ['Device', deviceName(e)], ['User', actorName(e)],
                ...(e.actor?.email ? [['Email', e.actor.email]] : []),
                ['Time', new Date(e.created_at).toLocaleString()],
                ...details.map(d => [d.name, d.value]), ...(e.request_id ? [['Request ID', e.request_id]] : [])]
                .map(([name, value]) => <div key={name}><dt>{name}</dt><dd>{value}</dd></div>)}
            </div>}
          </div>
        })}
        {!filtered.length && <div className="empty">{events.length ? 'No audit events match these filters.' : 'No audit activity yet.'}</div>}
      </section>
    </>}
  </div>
}
