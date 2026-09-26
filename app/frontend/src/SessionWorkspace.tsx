import { useEffect, useRef, useState } from 'react'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import '@xterm/xterm/css/xterm.css'
import { SshSession } from './ssh-session'

function SessionTerminal({ session, visible }: { session: SshSession; visible: boolean }) {
  const node = useRef<HTMLDivElement>(null)
  const fit = useRef<FitAddon | null>(null)
  const term = useRef<Terminal | null>(null)

  useEffect(() => {
    if (!node.current) return
    const terminal = new Terminal({ cursorBlink: true, convertEol: true, fontSize: 14, scrollback: 5000, theme: { background: '#111318' } })
    const addon = new FitAddon()
    terminal.loadAddon(addon)
    terminal.open(node.current)
    term.current = terminal; fit.current = addon
    const detach = session.attach(data => terminal.write(data))
    const input = terminal.onData(data => session.sendData(data))
    const observer = new ResizeObserver(() => {
      if (!node.current?.getClientRects().length) return
      addon.fit()
      session.resize(terminal.cols, terminal.rows)
    })
    observer.observe(node.current)
    return () => { observer.disconnect(); input.dispose(); detach(); terminal.dispose(); term.current = null; fit.current = null }
  }, [session])

  useEffect(() => {
    if (!visible || !term.current) return
    // A hidden tab has zero dimensions; fit again when it becomes visible.
    const frame = requestAnimationFrame(() => {
      fit.current?.fit()
      if (term.current) { session.resize(term.current.cols, term.current.rows); term.current.focus() }
    })
    return () => cancelAnimationFrame(frame)
  }, [session, visible])

  return <div className="workspace-terminal" ref={node} aria-label={`${session.name} SSH terminal`} />
}

export default function SessionWorkspace({ sessions, activeId, select, close }: {
  sessions: SshSession[]; activeId: string | null; select: (id: string) => void; close: (id: string) => void
}) {
  const [, setRevision] = useState(0)
  const tabRefs = useRef(new Map<string, HTMLButtonElement>())
  useEffect(() => {
    const unsubscribers = sessions.map(session => session.subscribe(() => setRevision(value => value + 1)))
    return () => unsubscribers.forEach(unsubscribe => unsubscribe())
  }, [sessions])

  function closeTab(id: string) {
    const index = sessions.findIndex(session => session.id === id)
    const next = (activeId !== id && sessions.find(session => session.id === activeId)) || sessions[index + 1] || sessions[index - 1]
    close(id)
    if (next) requestAnimationFrame(() => tabRefs.current.get(next.id)?.focus())
    else requestAnimationFrame(() => document.querySelector<HTMLButtonElement>('.sidebar nav button.active')?.focus())
  }

  if (!sessions.length) return null
  return <section className={`session-workspace ${activeId ? 'workspace-open' : ''}`} aria-label="Remote sessions">
    <div className="session-tabs" role="tablist" aria-label="Active SSH sessions">
      {sessions.map(session => <div className={`session-tab ${activeId === session.id ? 'selected' : ''}`} key={session.id}>
        <button type="button" role="tab" aria-selected={activeId === session.id} aria-controls={`session-${session.id}`}
          ref={node => { if (node) tabRefs.current.set(session.id, node); else tabRefs.current.delete(session.id) }}
          onClick={() => select(session.id)}>
          <span className={`session-dot ${session.state}`} aria-hidden="true" />{session.name} <small>SSH · {session.state}</small>
        </button>
        <button type="button" className="session-tab-close" aria-label={`Close ${session.name} SSH session`} onClick={() => closeTab(session.id)}>×</button>
      </div>)}
    </div>
    {sessions.map(session => <div id={`session-${session.id}`} role="tabpanel" aria-label={`${session.name} SSH session`}
      className="session-view" key={session.id} hidden={activeId !== session.id}>
      <div className="session-heading"><div><strong>{session.name}</strong><span className="muted">{session.platform} · SSH</span>
        <span className={`session-state ${session.state}`}><i />{session.state}</span></div>
        <button className="button" onClick={() => closeTab(session.id)}>Disconnect</button></div>
      {session.error && <p className="session-error" role="alert">{session.error}</p>}
      <SessionTerminal session={session} visible={activeId === session.id} />
      <div className="session-footer">Jump agent · SSH localhost:22</div>
    </div>)}
  </section>
}
