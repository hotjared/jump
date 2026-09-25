export type Platform = 'linux' | 'windows'
export type AgentDownloads = {
  version: string | null
  downloads: Partial<Record<Platform, string>>
  checksums: string | null
}

export const agentFilename: Record<Platform, string> = {
  linux: 'jump-agent-linux-amd64',
  windows: 'jump-agent-windows-amd64.exe',
}

export function downloadFor(release: AgentDownloads, platform: Platform): string | undefined {
  return release.downloads[platform]
}

export function enrollmentCommand(platform: Platform, server: string, token: string): string {
  const filename = agentFilename[platform]
  if (platform === 'windows') {
    return `.\\${filename} enroll --server ${server} --token ${token}\n.\\${filename} service install`
  }
  return `chmod +x ${filename}\nsudo ./${filename} enroll --server ${server} --token ${token}\nsudo ./${filename} service install`
}
