// @vitest-environment jsdom
import { afterEach, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import FilesPanel, { childPath, parentPath } from './FilesPanel'

afterEach(() => { cleanup(); vi.unstubAllGlobals() })
const device = { id: 'device-1', os_family: 'linux', online: true, identity_state: 'active', capabilities: ['file_transfer_v1'] }

it('navigates directories and offers explicit file download', async () => {
  const fetch = vi.fn(async (url: string) => ({ ok: true, json: async () => url.includes('path=%2Ffolder')
    ? { entries: [{ name: 'data.txt', type: 'file', size: 12, modified_at: '' }], more: false }
    : url.includes('/file-transfers') ? [] : { entries: [{ name: 'folder', type: 'directory', size: 0, modified_at: '' }], more: false } }))
  vi.stubGlobal('fetch', fetch)
  render(<FilesPanel device={device} csrf="token" />)
  fireEvent.click(await screen.findByRole('button', { name: /folder/i }))
  await screen.findByRole('button', { name: /data.txt/i })
  expect(screen.queryByRole('link', { name: 'Download' })).toBeNull()
  fireEvent.click(screen.getByRole('button', { name: /data.txt/i }))
  expect(screen.getByRole('link', { name: 'Download' }).getAttribute('href')).toContain('path=%2Ffolder%2Fdata.txt')
  fireEvent.click(screen.getByRole('button', { name: /Parent/i }))
  await waitFor(() => expect(screen.getByRole('button', { name: /folder/i })).toBeTruthy())
})

it('shows the older agent state without requesting files', () => {
  const fetch = vi.fn(); vi.stubGlobal('fetch', fetch)
  render(<FilesPanel device={{ ...device, capabilities: [] }} csrf="token" />)
  expect(screen.getByText('Update the Jump agent to enable file transfer.')).toBeTruthy()
  expect(fetch).not.toHaveBeenCalled()
})

it('handles Windows drive and Linux root parent paths', () => {
  expect(childPath('', 'C:\\', true)).toBe('C:\\')
  expect(childPath('C:\\', 'Users', true)).toBe('C:\\Users')
  expect(parentPath('C:\\Users', true)).toBe('C:\\')
  expect(parentPath('C:\\', true)).toBe('')
  expect(parentPath('/var/log', false)).toBe('/var')
  expect(parentPath('/', false)).toBe('/')
})

it('shows Windows fixed drives in This PC and opens a selected drive', async () => {
  const fetch = vi.fn(async (url: string) => ({ ok: true, json: async () => url.includes('path=C%3A%5C')
    ? { entries: [{ name: 'Users', type: 'directory', size: 0, modified_at: '' }], more: false }
    : url.includes('/file-transfers') ? [] : { entries: [{ name: 'C:\\', type: 'directory', size: 0, modified_at: '' }], more: false } }))
  vi.stubGlobal('fetch', fetch)
  render(<FilesPanel device={{ ...device, os_family: 'windows' }} csrf="token" />)
  expect(screen.getByText('This PC')).toBeTruthy()
  fireEvent.click(await screen.findByRole('button', { name: /C:/i }))
  await screen.findByRole('button', { name: /Users/i })
  expect(fetch).toHaveBeenCalledWith(expect.stringContaining('path=C%3A%5C'), expect.anything())
})
