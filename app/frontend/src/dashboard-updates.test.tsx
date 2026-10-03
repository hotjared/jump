// @vitest-environment jsdom
import { afterEach, expect, it, vi } from 'vitest'
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import Dashboard, { type DashboardDevice } from './Dashboard'
import AgentUpdatePanel from './AgentUpdatePanel'
import { useAgentUpdates } from './agent-updates'

const base = (id: string): DashboardDevice => ({ id, hostname: id, display_name: null, online: true, identity_state: 'active',
  agent_update: { current_version: 'v1', latest_version: 'v2', update_available: true, remote_update_supported: true, update_state: null } })
const request = vi.fn<(id: string) => Promise<{ id: string }>>()
const refresh = vi.fn<() => Promise<void>>()
function Harness({ devices, overview = false }: { devices: DashboardDevice[]; overview?: boolean }) {
  const updates = useAgentUpdates(devices, request, refresh)
  return <><Dashboard devices={devices} sessions={[]} events={[]} open={() => {}} admin updates={updates} />
    {overview && <AgentUpdatePanel info={{ ...devices[0].agent_update, remote_update_supported: true, update_state: null }} name="one" online active admin busy={updates.running(devices[0])} update={() => void updates.start(devices[0])} />}</>
}
afterEach(() => { cleanup(); vi.restoreAllMocks(); request.mockReset(); refresh.mockReset() })

it('starts an individual update, prevents duplicate requests, and reconciles the reconnect', async () => {
  request.mockResolvedValue({ id: 'op-one' }); refresh.mockResolvedValue(undefined)
  const devices = [base('one')]
  const view = render(<Harness devices={devices} />)
  const button = screen.getByRole('button', { name: 'Update one' })
  fireEvent.click(button); fireEvent.click(button)
  await waitFor(() => expect(request).toHaveBeenCalledExactlyOnceWith('one'))
  expect(button.textContent).toBe('Updating…')
  expect((button as HTMLButtonElement).disabled).toBe(true)
  await waitFor(() => expect(refresh).toHaveBeenCalled())
  view.rerender(<Harness devices={[{ ...base('one'), agent_update: { ...base('one').agent_update, current_version: 'v2', update_available: false,
    update_state: { id: 'op-one', state: 'completed', failure_reason: null } } }]} />)
  expect(screen.queryByRole('button', { name: 'Update one' })).toBeNull()
})

it('bulk updates all eligible devices independently and reports honest offline skips and failures', async () => {
  request.mockImplementation(async id => { if (id === 'bad') throw Error('Could not deliver update'); return { id: `op-${id}` } })
  refresh.mockResolvedValue(undefined)
  const devices = [base('one'), base('two'), base('bad'), { ...base('offline'), online: false },
    { ...base('current'), agent_update: { ...base('current').agent_update, update_available: false } },
    { ...base('manual'), agent_update: { ...base('manual').agent_update, remote_update_supported: false } },
    { ...base('busy'), agent_update: { ...base('busy').agent_update, update_state: { id: 'old', state: 'installing', failure_reason: null } } },
    { ...base('revoked'), identity_state: 'revoked' }]
  const view = render(<Harness devices={devices} />)
  fireEvent.click(screen.getByRole('button', { name: 'Update all' }))
  await waitFor(() => expect(request.mock.calls.map(([id]) => id).sort()).toEqual(['bad', 'one', 'two']))
  expect(await screen.findByText('Could not deliver update')).toBeTruthy()
  expect(screen.getByText(/Updating 2 of 3 agents/).textContent).toContain('1 offline skipped')
  expect(screen.getByText(/Updating 2 of 3 agents/).textContent).toContain('1 require manual update')
  expect(screen.getByText(/Updating 2 of 3 agents/).textContent).toContain('1 already updating')
  view.rerender(<Harness devices={devices.map(d => ['one', 'two'].includes(d.id) ? { ...d, agent_update: { ...d.agent_update,
    current_version: 'v2', update_available: false, update_state: { id: `op-${d.id}`, state: 'completed', failure_reason: null } } } : d)} />)
  expect(await screen.findByText(/2 updated · 1 failed/)).toBeTruthy()
  expect(screen.getByText('Offline · update when reconnected')).toBeTruthy()
})

it('keeps the existing device-details update entry point on the shared flow', async () => {
  vi.spyOn(window, 'confirm').mockReturnValue(true)
  request.mockResolvedValue({ id: 'op-one' }); refresh.mockResolvedValue(undefined)
  render(<Harness devices={[base('one')]} overview />)
  fireEvent.click(screen.getByRole('button', { name: 'Update agent' }))
  await act(async () => {})
  expect(request).toHaveBeenCalledExactlyOnceWith('one')
  expect((screen.getByRole('button', { name: 'Update one' }) as HTMLButtonElement).disabled).toBe(true)
})

it('does not treat a previous failed operation as failure of a new request', async () => {
  request.mockResolvedValue({ id: 'new-op' }); refresh.mockResolvedValue(undefined)
  const device = { ...base('one'), agent_update: { ...base('one').agent_update, update_state: { id: 'old-op', state: 'failed', failure_reason: 'old failure' } } }
  const view = render(<Harness devices={[device]} />)
  fireEvent.click(screen.getByRole('button', { name: 'Update one' }))
  await act(async () => {})
  view.rerender(<Harness devices={[{ ...device }]} />)
  expect(screen.getByRole('button', { name: 'Update one' }).textContent).toBe('Updating…')
  view.rerender(<Harness devices={[{ ...device, agent_update: { ...device.agent_update, update_state: { id: 'new-op', state: 'failed', failure_reason: 'checksum_mismatch' } } }]} />)
  expect(await screen.findByRole('alert')).toBeTruthy()
  expect(screen.getByRole('button', { name: 'Update one' }).textContent).toBe('Update')
})
