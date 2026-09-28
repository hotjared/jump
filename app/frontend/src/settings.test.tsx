// @vitest-environment jsdom
import { afterEach, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen, within } from '@testing-library/react'
import App from './App'

vi.mock('./SessionWorkspace', () => ({ default: () => null }))

const response = (data: unknown) => ({ ok: true, json: async () => data })
afterEach(() => { cleanup(); vi.unstubAllGlobals() })

async function openSettings(targetAgentVersion: string | null) {
  const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
    const path = String(input)
    if (path === '/api/me') return response({ id: 'user', email: 'user@example.com', display_name: 'User', role: 'user', csrf: 'csrf' })
    if (path === '/api/groups') return response([{ id: 'group', name: 'Production' }])
    if (path === '/api/tags') return response([{ id: 'tag', name: 'Critical' }])
    if (path === '/api/system-info') return response({ server_version: 'v0.1.6', target_agent_version: targetAgentVersion })
    if (path === '/api/devices') return response([])
    throw new Error(`Unexpected request: ${path}`)
  })
  vi.stubGlobal('fetch', fetchMock)
  render(<App />)
  expect(await screen.findByRole('button', { name: /Credentials/ })).toBeTruthy()
  fireEvent.click(await screen.findByRole('button', { name: /Settings/ }))
  return fetchMock
}

it('shows running and target versions alongside existing Groups and Tags', async () => {
  const fetchMock = await openSettings('v0.1.5')
  const system = await screen.findByRole('region', { name: 'System' })
  expect(within(system).getByText('Jump version').nextElementSibling?.textContent).toBe('v0.1.6')
  expect(within(system).getByText('Target agent version').nextElementSibling?.textContent).toBe('v0.1.5')
  expect(screen.getByText('Production')).toBeTruthy()
  expect(screen.getByText('Critical')).toBeTruthy()
  expect(fetchMock).toHaveBeenCalledWith('/api/system-info', { credentials: 'same-origin' })
})

it('shows Not configured when no target agent release is set', async () => {
  await openSettings(null)
  const system = await screen.findByRole('region', { name: 'System' })
  expect(within(system).getByText('Not configured')).toBeTruthy()
})
