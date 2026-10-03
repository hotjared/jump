import { useEffect, useRef, useState } from 'react'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import '@xterm/xterm/css/xterm.css'
import { SshSession } from './ssh-session'
import { RdpSession } from './rdp-session'
import { ScreenSession } from './screen-session'
import RemoteNotice, { useRemoteNotice } from './RemoteNotice'

export type WorkspaceSession = SshSession | RdpSession | ScreenSession

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

  return <div className="workspace-terminal">
    <div className="workspace-terminal-viewport" ref={node} aria-label={`${session.name} SSH terminal`} />
  </div>
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

function SessionScreen({ session, visible }: { session: ScreenSession; visible: boolean }) {
  const node = useRef<HTMLDivElement>(null)
  const active = useRef(visible)
  active.current = visible
  useEffect(() => { if (node.current) return session.attach(node.current, () => active.current) }, [session])
  useEffect(() => { if (!visible) session.release() }, [visible, session])
  return <div className="workspace-desktop" ref={node} aria-label={`${session.name} Screen desktop`} />
}

function RdpActions({ session, notify }: { session: RdpSession | ScreenSession; notify: (text: string) => void }) {
  async function transfer(action: 'paste' | 'copy') {
    const success = action === 'paste'
      ? await session.pasteLocalClipboardToRemote()
      : await session.copyRemoteClipboardToLocal()
    if (success) notify(action === 'paste' ? 'Sent to Remote' : 'Copied from Remote')
  }
  return <>
    <button type="button" className="button" disabled={session.state !== 'connected' || session.protocol === 'Screen' && (!session.canControl || session.operationBusy)} onClick={() => void transfer('paste')}>Paste to Remote</button>
    <button type="button" className="button" disabled={session.state !== 'connected' || !session.hasRemoteClipboard} onClick={() => void transfer('copy')}>Copy from Remote</button>
  </>
}

function SessionView({ session, visible, fileDevices, openFiles, close }: {
  session: WorkspaceSession; visible: boolean; fileDevices: string[]; openFiles: (id: string) => void; close: () => void
}) {
  const { notice, notify, dismiss } = useRemoteNotice()
  const clipboardError = session.protocol === 'SSH' ? '' : session.clipboardError
  const operationMessage = session.protocol === 'Screen' ? session.operationMessage : ''
  const clipboardNoticeVersion = session.protocol === 'SSH' ? 0 : session.clipboardNoticeVersion
  const operationNoticeVersion = session.protocol === 'Screen' ? session.operationNoticeVersion : 0
  const operationError = session.protocol === 'Screen' && session.operationError
  useEffect(() => { if (clipboardError) notify(clipboardError, 'error') }, [clipboardError, clipboardNoticeVersion, notify])
  useEffect(() => { if (operationMessage) notify(operationMessage, operationError ? 'error' : 'info') }, [operationMessage, operationError, operationNoticeVersion, notify])
  return <div id={`session-${session.id}`} role="tabpanel" aria-label={`${session.name} ${session.protocol} session`}
    className="session-view" hidden={!visible}>
    <div className="session-heading"><div><strong>{session.name}</strong><span className="muted">{session.platform} · {session.protocol}</span>
      <span className={`session-state state-${session.state}`}><i />{session.state}</span></div>
      <div className="session-actions">{fileDevices.includes(session.deviceId) && <button className="button" onClick={() => openFiles(session.deviceId)}>Files</button>}{session.protocol === 'Screen' && <><button className="button" disabled={session.state !== 'connected'} onClick={() => session.setMode(session.mode === 'control' ? 'view' : 'control')}>{session.mode === 'control' ? 'Control · switch to View Only' : 'View Only · switch to Control'}</button>{session.supportsAdminOperations ? <><RdpActions session={session} notify={notify} /><button className="button" disabled={!session.canControl || session.operationBusy} onClick={() => void session.sendSAS()}>Ctrl+Alt+Del</button></> : <span className="muted">Update the Windows Jump agent to enable unattended admin controls.</span>}<button className="button" onClick={e => e.currentTarget.closest('.session-view')?.requestFullscreen()}>Fullscreen</button></>}{session.protocol === 'RDP' && <><RdpActions session={session} notify={notify} /><button className="button" onClick={e => e.currentTarget.closest('.session-view')?.requestFullscreen()}>Fullscreen</button></>}
        <button className="button" onClick={close}>Disconnect</button></div></div>
    {session.error && <p className="session-error" role="alert">{session.error}</p>}
    <div className="remote-session-area">
      <RemoteNotice notice={notice} dismiss={dismiss} />
      {session.protocol === 'SSH' ? <SessionTerminal session={session} visible={visible} /> : session.protocol === 'Screen' ? <SessionScreen session={session} visible={visible} /> : <SessionDesktop session={session} visible={visible} />}
    </div>
    <div className="session-footer">Jump agent · {session.protocol === 'SSH' ? 'SSH localhost:22' : session.protocol === 'Screen' ? 'Windows console desktop · primary display' : 'RDP localhost:3389'}</div>
  </div>
}

export default function SessionWorkspace({ sessions, activeId, select, close, fileDevices = [], openFiles = () => {} }: {
  sessions: WorkspaceSession[]; activeId: string | null; select: (id: string) => void; close: (id: string) => void;
  fileDevices?: string[]; openFiles?: (deviceId: string) => void
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
          <span className={`session-dot state-${session.state}`} aria-hidden="true" />{session.name} <small>{session.protocol} · {session.state}</small>
        </button>
        <button type="button" className="session-tab-close" aria-label={`Close ${session.name} ${session.protocol} session`} onClick={() => closeTab(session.id)}>×</button>
      </div>)}
    </div>
    {sessions.map(session => <SessionView key={session.id} session={session} visible={activeId === session.id}
      fileDevices={fileDevices} openFiles={openFiles} close={() => closeTab(session.id)} />)}
  </section>
}
