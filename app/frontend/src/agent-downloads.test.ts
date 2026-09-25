import { describe, expect, it } from 'vitest'
import { downloadFor, enrollmentCommand, type AgentDownloads } from './agent-downloads'

const release: AgentDownloads = {
  version: 'v1.2.3',
  downloads: {
    linux: 'https://github.com/hotjared/jump/releases/download/v1.2.3/jump-agent-linux-amd64',
    windows: 'https://github.com/hotjared/jump/releases/download/v1.2.3/jump-agent-windows-amd64.exe',
  },
  checksums: 'https://github.com/hotjared/jump/releases/download/v1.2.3/SHA256SUMS',
}

describe('native agent enrollment', () => {
  it('selects the matching binary without including the token in download URLs', () => {
    expect(downloadFor(release, 'linux')).toMatch(/jump-agent-linux-amd64$/)
    expect(downloadFor(release, 'windows')).toMatch(/jump-agent-windows-amd64\.exe$/)
    for (const url of [...Object.values(release.downloads), release.checksums]) {
      expect(url).not.toContain('one-use-secret')
    }
  })
  it('renders runnable commands using the published filenames', () => {
    expect(enrollmentCommand('linux', 'https://agent.example.com', 'one-use-secret')).toBe(
      'chmod +x jump-agent-linux-amd64\nsudo ./jump-agent-linux-amd64 enroll --server https://agent.example.com --token one-use-secret\nsudo ./jump-agent-linux-amd64 run',
    )
    expect(enrollmentCommand('windows', 'https://agent.example.com', 'one-use-secret')).toBe(
      '.\\jump-agent-windows-amd64.exe enroll --server https://agent.example.com --token one-use-secret\n.\\jump-agent-windows-amd64.exe run',
    )
  })
})
