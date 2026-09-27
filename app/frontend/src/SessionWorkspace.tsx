import { useEffect, useRef, useState } from 'react'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import '@xterm/xterm/css/xterm.css'
import { SshSession } from './ssh-session'
import { RdpSession } from './rdp-session'

export type WorkspaceSession = SshSession | RdpSession

const MIN_COLUMNS = 20
const MAX_COLUMNS = 500
const MIN_ROWS = 5
const MAX_ROWS = 200

function SessionTerminal({ session, visible }: { session: SshSession; visible: boolean }) {
  const node = useRef<HTMLDivElement>(null)
  const fit = useRef<FitAddon | null>(null)
  const term = useRef<Terminal | null>(null)
  const visibleRef = useRef(visible)
  const frame = useRef<number | null>(null)
  const lastSize = useRef<{ columns: number; rows: number } | null>(null)

  visibleRef.current = visible

  useEffect(() => {
    if (!node.current) return
    const terminal = new Terminal({ cursorBlink: true, convertEol: true, fontSize: 14, scrollback: 5000, theme: { background: '#111318' } })
    const addon = new FitAddon()
    terminal.loadAddon(addon)
    terminal.open(node.current)
    term.current = terminal; fit.current = addon
    const detach = session.attach(data => terminal.write(data))
    const input = terminal.onData(data => session.sendData(data))

    const scheduleFit = () => {
      if (!visibleRef.current || frame.current !== null) return
      frame.current = -1
      const id = requestAnimationFrame(() => {
        frame.current = null
        if (!visibleRef.current || !node.current?.getClientRects().length) return
        addon.fit()
        const columns = terminal.cols
        const rows = terminal.rows
        if (columns < MIN_COLUMNS || columns > MAX_COLUMNS || rows < MIN_ROWS || rows > MAX_ROWS) return
        if (lastSize.current?.columns === columns && lastSize.current.rows === rows) return
        lastSize.current = { columns, rows }
        session.resize(columns, rows)
      })
      if (frame.current !== null) frame.current = id
    }

    const observer = new ResizeObserver(scheduleFit)
    observer.observe(node.current)
    scheduleFit()

    return () => {
      observer.disconnect()
      if (frame.current !== null) cancelAnimationFrame(frame.current)
      frame.current = null
      input.dispose()
      detach()
      terminal.dispose()
      term.current = null
      fit.current = null
    }
  }, [session])

  useEffect(() => {
    if (!visible) return
    const id = requestAnimationFrame(() => {
      if (!visibleRef.current || !term.current || !node.current?.getClientRects().length) return
      fit.current?.fit()
      const columns = term.current.cols
      const rows = term.current.rows
      if (columns >= MIN_COLUMNS && columns <= MAX_COLUMNS && rows >= MIN_ROWS && rows <= MAX_ROWS &&
        (lastSize.current?.columns !== columns || lastSize.current.rows !== rows)) {
        lastSize.current = { columns, rows }
        session.resize(columns, rows)
      }
      term.current.focus()
    })
    return () => cancelAnimationFrame(id)
  }, [session, visible])

  return <div className="workspace-terminal" ref={node} aria-label={`${session.name} SSH terminal`} />
}

function SessionDesktop({ session, visible }: { session: RdpSession; visible: boolean }) {
  const node = useRef<HTMLDivElement>(null)
  const visibleRef = useRef(visible)
  visibleRef.current = visible
  useEffect(() => {
    if (!node.current) return
    return session.attach(node.current, () => visibleRef.current)
  }, [session])
  return <div className="workspace-desktop" ref={node} aria-label={`${session.name} RDP desktop`} />
}

export default function SessionWorkspace({ sessions, activeId, select, close }: {
  sessions: WorkspaceSession[]; activeId: string | null; select: (id: string) => void; close: (id: string) => void
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
    <div className="session-tabs" role="tablist" aria-label="Active remote sessions">
      {sessions.map(session => <div className={`session-tab ${activeId === session.id ? 'selected' : ''}`} key={session.id}>
        <button type="button" role="tab" aria-selected={activeId === session.id} aria-controls={`session-${session.id}`}
          ref={node => { if (node) tabRefs.current.set(session.id, node); else tabRefs.current.delete(session.id) }}
          onClick={() => select(session.id)}>
          <span className={`session-dot ${session.state}`} aria-hidden="true" />{session.name} <small>{session.protocol} · {session.state}</small>
        </button>
        <button type="button" className="session-tab-close" aria-label={`Close ${session.name} ${session.protocol} session`} onClick={() => closeTab(session.id)}>×</button>
      </div>)}
    </div>
    {sessions.map(session => <div id={`session-${session.id}`} role="tabpanel" aria-label={`${session.name} ${session.protocol} session`}
      className="session-view" key={session.id} hidden={activeId !== session.id}>
      <div className="session-heading"><div><strong>{session.name}</strong><span className="muted">{session.platform} · {session.protocol}</span>
        <span className={`session-state ${session.state}`}><i />{session.state}</span></div>
        <div className="session-actions">{session.protocol === 'RDP' && <>
          <button className="button" disabled={session.state !== 'connected'} onClick={() => void session.pasteLocalClipboardToRemote()}>Paste to Remote</button>
          <button className="button" disabled={session.state !== 'connected' || !session.hasRemoteClipboard} onClick={() => void session.copyRemoteClipboardToLocal()}>Copy from Remote</button>
          <button className="button" onClick={e => e.currentTarget.closest('.session-view')?.requestFullscreen()}>Fullscreen</button>
        </>}
          <button className="button" onClick={() => closeTab(session.id)}>Disconnect</button></div></div>
      {session.protocol === 'RDP' && session.clipboardStatus && <span className="muted" role="status">{session.clipboardStatus}</span>}
      {session.protocol === 'RDP' && session.clipboardError && <p className="session-error" role="alert">{session.clipboardError}</p>}
      {session.error && <p className="session-error" role="alert">{session.error}</p>}
      {session.protocol === 'SSH' ? <SessionTerminal session={session} visible={activeId === session.id} /> : <SessionDesktop session={session} visible={activeId === session.id} />}
      <div className="session-footer">Jump agent · {session.protocol === 'SSH' ? 'SSH localhost:22' : 'RDP localhost:3389'}</div>
    </div>)}
  </section>
}
