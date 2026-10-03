// @vitest-environment jsdom
import { afterEach, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen, within } from '@testing-library/react'
import App from './App'
import { enrollmentCommand, type Platform } from './agent-downloads'

vi.mock('./SessionWorkspace', () => ({ default: () => null }))
afterEach(() => { cleanup(); vi.unstubAllGlobals() })

it.each(['windows', 'linux'] as Platform[])('copies exact token and complete %s commands', async platform => {
  const token = 'single-use-token-exactly-as-issued'
  const agent_url = 'https://agent.example'
  const writeText = vi.fn().mockResolvedValue(undefined)
  vi.stubGlobal('navigator', { clipboard: { writeText } })
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
    const path = String(input)
    const data = path === '/api/me' ? { id: 'u', display_name: 'Admin', role: 'admin', csrf: 'csrf' }
      : path === '/api/enrollment-tokens' ? { token, agent_url, expires_at: new Date().toISOString() }
      : path === '/api/agent-downloads' ? { version: 'v1', downloads: {}, checksums: null }
      : []
    return { ok: true, json: async () => data }
  }))
  render(<App />)
  fireEvent.click(await screen.findByRole('button', { name: /Enroll device/ }))
  if (platform === 'windows') fireEvent.click(screen.getByRole('button', { name: /Windows amd64/ }))
  fireEvent.click(screen.getByRole('button', { name: 'Generate token' }))
  const code = await screen.findByRole('button', { name: 'Copy enrollment code' })
  fireEvent.click(code)
  expect(writeText).toHaveBeenLastCalledWith(token)
  const commands = screen.getByRole('button', { name: 'Copy commands' })
  fireEvent.click(commands)
  expect(writeText).toHaveBeenLastCalledWith(enrollmentCommand(platform, agent_url, token))
  expect(commands.parentElement?.querySelector('pre')?.textContent).toBe(enrollmentCommand(platform, agent_url, token))
  expect(await within(commands).findByText('Copied')).toBeTruthy()
  expect(code.getAttribute('type')).toBe('button')
})
