export type UpdateInfo = {
  current_version: string;
  latest_version: string | null;
  update_available: boolean;
  remote_update_supported: boolean;
  update_state: { state: string; target_version: string; failure_reason: string | null } | null;
}

const progress: Record<string, string> = {
  pending: 'Update requested…', downloading: 'Downloading update…',
  installing: 'Installing…', restarting: 'Restarting agent… Waiting for it to reconnect…',
}

export default function AgentUpdatePanel({ info, name, online, active, admin, busy, update }:
  { info: UpdateInfo; name: string; online: boolean; active: boolean; admin: boolean; busy: boolean; update: () => void }) {
  const state = info.update_state
  const running = !!state && state.state in progress
  if (!info.update_available && !running && state?.state !== 'failed') return null

  return <section className="identity-control" aria-label="Agent update">
    <h3>Agent update</h3>
    <p>Agent version: {info.current_version || 'Unknown'}</p>
    <p>Latest available version: {info.latest_version || 'Unknown'}</p>
    {running ? <p role="status">{progress[state.state]}</p>
      : state?.state === 'failed' ? <p role="alert">Update failed: {state.failure_reason?.replaceAll('_', ' ') || 'unknown error'}. Current version: {info.current_version || 'Unknown'}.</p>
      : state?.state === 'completed' ? <p role="status">Update successful.</p> : null}
    {info.update_available && !running ? <>
      <p>Update available: {info.current_version} → {info.latest_version}</p>
      {!info.remote_update_supported ? <p>This agent must be updated manually once before remote updates are supported.</p>
        : admin ? <button className="button" disabled={busy || !online || !active} onClick={() => {
          if (window.confirm(`Update ${name} from ${info.current_version} to ${info.latest_version}?\n\nThe Jump agent service will restart briefly.`)) update()
        }}>Update agent</button> : null}
    </> : null}
  </section>
}
