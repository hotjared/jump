import { useEffect, useRef, useState } from 'react'
import { progress } from './AgentUpdatePanel'

export type UpdateDevice = {
  id: string; online: boolean; identity_state: string;
  agent_update: { update_available: boolean; latest_version: string | null; current_version: string;
    remote_update_supported?: boolean;
    update_state: { id?: string; state: string; failure_reason: string | null } | null };
}
type Attempt = { state: 'requesting' | 'waiting' | 'completed' | 'failed'; target: string | null; id?: string; error?: string }
export const updateRunning = (device: UpdateDevice) => !!device.agent_update.update_state && device.agent_update.update_state.state in progress
export const updateEligible = (device: UpdateDevice) => device.online && device.identity_state === 'active' &&
  device.agent_update.update_available && device.agent_update.remote_update_supported === true && !updateRunning(device)

export function useAgentUpdates(devices: UpdateDevice[], request: (id: string) => Promise<{ id: string; target_version?: string }>, refresh: () => Promise<void>) {
  const [attempts, setAttempts] = useState<Record<string, Attempt>>({})
  const attemptsRef = useRef(attempts)
  const [batch, setBatch] = useState<{ ids: string[]; offline: number; unsupported: number; running: number } | null>(null)
  function record(id: string, attempt: Attempt) {
    attemptsRef.current = { ...attemptsRef.current, [id]: attempt }
    setAttempts(attemptsRef.current)
  }
  const running = (device: UpdateDevice) => ['requesting', 'waiting'].includes(attemptsRef.current[device.id]?.state) || updateRunning(device)
  useEffect(() => {
    for (const [id, attempt] of Object.entries(attemptsRef.current)) {
      if (attempt.state !== 'waiting') continue
      const device = devices.find(d => d.id === id)
      if (!device) { record(id, { ...attempt, state: 'failed', error: 'Device is no longer available.' }); continue }
      const op = device.agent_update.update_state
      if (op?.id === attempt.id && op?.state === 'failed') record(id, { ...attempt, state: 'failed', error: op.failure_reason?.replaceAll('_', ' ') || 'Update failed.' })
      else if (device.online && device.agent_update.current_version === attempt.target && op?.id === attempt.id && op?.state === 'completed')
        record(id, { ...attempt, state: 'completed' })
    }
  }, [devices])
  async function start(device: UpdateDevice) {
    if (!updateEligible(device) || running(device)) return
    record(device.id, { state: 'requesting', target: device.agent_update.latest_version })
    try {
      const op = await request(device.id)
      record(device.id, { state: 'waiting', target: op.target_version || device.agent_update.latest_version, id: op.id })
      // A refresh failure doesn't mean the accepted update failed. Normal polling
      // reconciles against the same operation after the agent reconnects.
      await refresh().catch(() => {})
    } catch (error) {
      record(device.id, { state: 'failed', target: device.agent_update.latest_version, error: error instanceof Error ? error.message : 'Could not request update.' })
    }
  }
  async function startAll() {
    const outdated = devices.filter(d => d.identity_state === 'active' && d.agent_update.update_available)
    const eligible = outdated.filter(d => updateEligible(d) && !running(d))
    setBatch({ ids: eligible.map(d => d.id), offline: outdated.filter(d => !d.online).length,
      unsupported: outdated.filter(d => d.online && d.agent_update.remote_update_supported !== true).length,
      running: outdated.filter(d => d.online && running(d)).length })
    await Promise.allSettled(eligible.map(start))
  }
  const completed = batch?.ids.filter(id => attempts[id]?.state === 'completed').length || 0
  const failed = batch?.ids.filter(id => attempts[id]?.state === 'failed').length || 0
  const pending = (batch?.ids.length || 0) - completed - failed
  const skipped = batch ? [batch.offline && `${batch.offline} offline skipped`, batch.unsupported && `${batch.unsupported} require manual update`, batch.running && `${batch.running} already updating`].filter(Boolean).join(' · ') : ''
  const status = batch ? [pending ? `Updating ${pending} of ${batch.ids.length} agents…` : `${completed} updated · ${failed} failed`, skipped].filter(Boolean).join(' · ') : ''
  return { start, startAll, running, attempts, status, batchRunning: pending > 0 }
}

export type AgentUpdates = ReturnType<typeof useAgentUpdates>
