import { useCallback, useEffect, useState } from 'react'
import { label, reason, safeDetails, type AuditEvent } from './audit-events'
import './diagnostics.css'

type Component = { status: 'healthy' | 'unavailable' | 'unknown'; detail: string }
type Counts = { devices_online: number; active_sessions: number; active_file_transfers: number; active_agent_updates: number }
type BrokerCounts = { agent_connections: number; session_routes: number; file_routes: number; updates_in_progress: number }
type Failure = Pick<AuditEvent, 'id' | 'event_type' | 'created_at' | 'detail' | 'request_id'> & { device_name: string | null }
type RemoteSessionTrace = {
  id: string; protocol: 'ssh' | 'rdp'; state: 'connecting' | 'active' | 'closed' | 'failed';
  device: { id: string | null; name: string }; created_at: string; attached_at: string | null;
  closed_at: string | null; failure_reason: string | null; request_id: string | null;
  stages: { stage: string; created_at: string }[];
}
const stageLabels: Record<string, string> = {
  session_created: 'Session created', browser_attached: 'Browser attached', broker_connected: 'Broker route opened',
  agent_stream_opened: 'Agent SSH stream opened', agent_tunnel_opened: 'Agent TCP tunnel opened',
  host_key_verified: 'Host key verified', guacd_connected: 'guacd connected',
  guacd_handshake_started: 'RDP handshake started', protocol_ready: 'Protocol ready',
  session_active: 'Session active', session_closed: 'Session closed', session_failed: 'Session failed',
}
const clock = (value: string) => new Date(value).toLocaleTimeString(undefined, { hour: 'numeric', minute: '2-digit', second: '2-digit' })
const duration = (start: string, end: string | null) => {
  const seconds = Math.max(0, Math.round((new Date(end || Date.now()).getTime() - new Date(start).getTime()) / 1000))
  return Number.isFinite(seconds) ? `${seconds}s` : '—'
}
export type DiagnosticsResult = {
  components: Record<'api' | 'database' | 'broker' | 'guacd', Component>;
  runtime: Counts | null; broker_runtime: BrokerCounts | null; recent_failures: Failure[];
}
const componentNames = { api: 'Jump API', database: 'Database', broker: 'Broker', guacd: 'guacd' }
const runtimeLabels: [keyof Counts, string][] = [
  ['devices_online', 'Devices online'], ['active_sessions', 'Active remote sessions'],
  ['active_file_transfers', 'Active file transfers'], ['active_agent_updates', 'Agent updates in progress'],
]
const brokerLabels: [keyof BrokerCounts, string][] = [
  ['agent_connections', 'Broker agent connections'], ['session_routes', 'Broker session routes'],
  ['file_routes', 'Broker file routes'], ['updates_in_progress', 'Broker updates in progress'],
]

export default function Diagnostics({ admin }: { admin: boolean }) {
  const [result, setResult] = useState<DiagnosticsResult | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(false)
  const [sessions, setSessions] = useState<RemoteSessionTrace[]>([])
  const [sessionsError, setSessionsError] = useState(false)
  const refresh = useCallback(async (signal?: AbortSignal) => {
    setBusy(true); setError(false); setSessionsError(false)
    try {
      await Promise.all([
        (async () => {
          try {
            const response = await fetch('/api/diagnostics', { credentials: 'same-origin', signal })
            if (!response.ok) throw new Error('Diagnostics request failed')
            const data = await response.json() as DiagnosticsResult
            if (!signal?.aborted) setResult(data)
          } catch {
            if (!signal?.aborted) setError(true)
          }
        })(),
        (async () => {
          try {
            const response = await fetch('/api/diagnostics/sessions', { credentials: 'same-origin', signal })
            if (!response.ok) throw new Error('Session diagnostics request failed')
            const recent = await response.json() as RemoteSessionTrace[]
            if (!signal?.aborted) setSessions(recent)
          } catch {
            if (!signal?.aborted) setSessionsError(true)
          }
        })(),
      ])
    } finally {
      if (!signal?.aborted) setBusy(false)
    }
  }, [])
  useEffect(() => {
    if (!admin) return
    const controller = new AbortController()
    void refresh(controller.signal)
    return () => controller.abort()
  }, [admin, refresh])
  return <div className="diagnostics">
    <div className="heading"><div><p className="eyebrow">DIAGNOSTICS</p><h1>System Diagnostics</h1><p>Current health and recent operational failures.</p></div>
      {admin && <button className="button" disabled={busy} onClick={() => void refresh()}>{busy ? 'Checking…' : 'Refresh'}</button>}</div>
    {!admin ? <section className="panel"><div className="empty">Admin access required.</div></section> : <>
      {error && <p role="alert" className="diagnostics-error">Could not refresh diagnostics. {result && 'Showing the last check.'}</p>}
      {!result && busy && <p>Checking components…</p>}
      {result && <>
        <section className="panel diagnostics-panel" aria-label="Core components"><h2>Core components</h2>
          {(Object.keys(componentNames) as (keyof typeof componentNames)[]).map(key => <div className="diagnostics-component" key={key}>
            <span className={`diagnostics-dot ${result.components[key].status}`} aria-hidden="true" /><strong>{componentNames[key]}</strong>
            <span>{result.components[key].status === 'healthy' ? 'Healthy' : result.components[key].status === 'unavailable' ? 'Unavailable' : 'Unknown'}</span>
            <small>{result.components[key].detail}</small>
          </div>)}
        </section>
        <section className="panel diagnostics-panel" aria-label="Runtime"><h2>Runtime</h2>
          <div className="diagnostics-counts">{runtimeLabels.map(([key, title]) => <div key={key}><small>{title}</small><strong>{result.runtime?.[key] ?? '—'}</strong></div>)}
            {result.broker_runtime && brokerLabels.map(([key, title]) => <div key={key}><small>{title}</small><strong>{result.broker_runtime?.[key]}</strong></div>)}
          </div>
          {!result.runtime && <p className="diagnostics-note">Runtime counts unavailable while the database is disconnected.</p>}
        </section>
        <section className="panel diagnostics-panel" aria-label="Recent remote sessions"><h2>Recent Remote Sessions</h2>
          {sessionsError && <p role="status" className="diagnostics-note">Session diagnostics unavailable.</p>}
          {sessions.length ? sessions.map(session => {
            const failure = reason(session.failure_reason)
            return <details className="diagnostics-session" key={session.id}>
              <summary><strong>{session.device?.name || 'Deleted device'}</strong><span>{session.protocol.toUpperCase()}</span>
                <span className={`diagnostics-state ${session.state}`}>{session.state}</span>
                <time dateTime={session.created_at}>{clock(session.created_at)}</time>
                <span>{duration(session.created_at, session.closed_at)}</span></summary>
              <div className="diagnostics-trace">
                {session.stages.length ? session.stages.filter(item => item.stage in stageLabels).map((item, index) =>
                  <div key={`${item.stage}-${index}`}><span aria-hidden="true">{item.stage === 'session_failed' ? '✕' : '✓'}</span>
                    <span>{item.stage === 'session_failed' && failure ? failure : stageLabels[item.stage]}</span>
                    <time dateTime={item.created_at}>{clock(item.created_at)}</time></div>
                ) : <p>No detailed trace available for this older session.</p>}
                <dl><div><dt>Session ID</dt><dd>{session.id}</dd></div>
                  {session.request_id && <div><dt>Request ID</dt><dd>{session.request_id}</dd></div>}
                  {failure && <div><dt>Failure reason</dt><dd>{failure}</dd></div>}
                </dl>
              </div>
            </details>
          }) : !sessionsError && <div className="empty">No recent remote sessions.</div>}
        </section>
        <section className="panel diagnostics-panel" aria-label="Recent failures"><h2>Recent Failures</h2>
          {result.recent_failures.length ? result.recent_failures.map(event => {
            const details = safeDetails({ ...event, device_id: null })
            const failureReason = details.find(item => item.key === 'reason')?.value || reason(event.detail?.reason)
            return <div className="diagnostics-failure" key={event.id}>
              <div><strong>{label(event.event_type)}</strong><span>{event.device_name || 'Unknown device'}</span>
                {failureReason && <small>{failureReason}</small>}</div>
              <time dateTime={event.created_at}>{new Date(event.created_at).toLocaleString()}</time>
              {(details.length > 0 || event.request_id) && <details><summary>Details</summary><dl>
                {details.map(item => <div key={item.key}><dt>{item.name}</dt><dd>{item.value}</dd></div>)}
                {event.request_id && <div><dt>Request ID</dt><dd>{event.request_id}</dd></div>}
              </dl></details>}
            </div>
          }) : <div className="empty">No recent failures.</div>}
        </section>
      </>}
    </>}
  </div>
}
