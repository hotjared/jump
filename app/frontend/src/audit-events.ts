export type AuditEvent = {
  id: string; event_type: string; created_at: string; device_id: string | null; actor_user_id?: string | null;
  actor?: { name: string; email: string } | null; device?: { id: string; name: string } | null;
  request_id?: string | null; detail?: Record<string, unknown> | null;
}

export const labels: Record<string, string> = {
  ssh_session_started: 'SSH session started', ssh_session_ended: 'SSH session closed',
  ssh_session_failed: 'SSH session failed', ssh_session_idle_timeout: 'SSH session idle timeout',
  rdp_session_started: 'RDP session started', rdp_session_ended: 'RDP session closed', rdp_session_failed: 'RDP session failed',
  agent_connected: 'Agent connected', agent_disconnected: 'Agent disconnected',
  agent_identity_revoked: 'Device revoked', agent_update_started: 'Agent update started',
  agent_update_completed: 'Agent updated', agent_update_failed: 'Agent update failed',
  device_enrolled: 'Device enrolled', device_updated: 'Device updated', device_deleted: 'Device deleted',
  file_upload_started: 'Upload started', file_upload_completed: 'Uploaded', file_upload_failed: 'Upload failed', file_upload_cancelled: 'Upload cancelled',
  file_download_started: 'Download started', file_download_completed: 'Downloaded', file_download_failed: 'Download failed', file_download_cancelled: 'Download cancelled',
  credential_created: 'Credential created', credential_updated: 'Credential updated', credential_deleted: 'Credential deleted',
  ssh_host_key_trusted: 'SSH host key trusted', ssh_host_key_reset: 'SSH host key reset',
  enrollment_token_created: 'Enrollment token created', enrollment_token_revoked: 'Enrollment token revoked', user_login: 'User signed in',
  group_created: 'Group created', group_updated: 'Group updated', group_deleted: 'Group deleted',
  tag_created: 'Tag created', tag_updated: 'Tag updated', tag_deleted: 'Tag deleted',
}
export const label = (type: string) => labels[type] || (type.length <= 64 && /^[a-z][a-z0-9_]*$/.test(type)
  ? type.replaceAll('_', ' ').replace(/^./, c => c.toUpperCase()) : 'Unknown activity')

const reasons: Record<string, string> = {
  guacd_disconnected: 'RDP protocol service disconnected', authentication_failed: 'Authentication failed',
  reconnect_timeout: 'Agent did not reconnect in time', checksum_mismatch: 'Download checksum did not match',
  agent_unavailable: 'Agent unavailable', device_disconnected: 'Device disconnected',
  idle_timeout: 'Session idle timeout', permission_denied: 'Permission denied',
}
export const reason = (value: unknown) => typeof value === 'string' && /^[a-z][a-z0-9_]{0,79}$/.test(value)
  ? reasons[value] || value.replaceAll('_', ' ') : null
export const filename = (value: unknown) => typeof value === 'string' && value.length > 0 && value.length <= 180 && !/[\\/\x00-\x1f\x7f]/.test(value)
  ? value : null

export type Category = 'sessions' | 'files' | 'agents' | 'credentials' | 'security' | 'organization' | 'other'
export function category(type: string): Category {
  if (/^(ssh|rdp)_session_/.test(type)) return 'sessions'
  if (type.startsWith('file_')) return 'files'
  if (type.startsWith('agent_') || type.startsWith('device_')) return 'agents'
  if (type.startsWith('credential_')) return 'credentials'
  if (type.startsWith('ssh_host_key_') || type.startsWith('enrollment_token_') || type === 'user_login') return 'security'
  if (type.startsWith('group_') || type.startsWith('tag_')) return 'organization'
  return 'other'
}

// The API strips unapproved fields. Keep the UI explicit too, for older or malformed responses.
const fieldsFor = (type: string) => {
  if (/^(ssh|rdp)_session_(started|ended|failed)$/.test(type)) return ['reason', 'session_id']
  if (type === 'ssh_session_idle_timeout') return ['session_id']
  if (/^file_(upload|download)_(started|completed|failed|cancelled)$/.test(type)) return ['filename', 'size', 'reason', 'transfer_id']
  if (/^agent_update_(started|completed|failed)$/.test(type)) return ['from_version', 'target_version', 'reason', 'operation_id']
  if (/^credential_(created|updated|deleted)$/.test(type)) return ['label', 'kind']
  if (type === 'ssh_host_key_trusted') return ['fingerprint', 'session_id']
  if (/^(group|tag)_(created|updated|deleted)$/.test(type)) return ['name']
  if (type === 'device_deleted') return ['display_name', 'hostname']
  return []
}
const fieldLabels: Record<string, string> = {
  reason: 'Reason', session_id: 'Session ID', filename: 'Filename', size: 'Size (bytes)',
  transfer_id: 'Transfer ID', from_version: 'From version', target_version: 'Target version',
  operation_id: 'Operation ID', label: 'Label', kind: 'Kind', fingerprint: 'Fingerprint', name: 'Name',
  display_name: 'Display name', hostname: 'Hostname',
}
export function safeDetails(event: AuditEvent): { key: string; name: string; value: string }[] {
  const detail = event.detail || {}
  return fieldsFor(event.event_type).flatMap(key => {
    const value = detail[key]
    const safe = key === 'reason' ? reason(value) : key === 'filename' ? filename(value) :
      key === 'fingerprint' ? (typeof value === 'string' && /^SHA256:[A-Za-z0-9+/=]{20,64}$/.test(value) ? value : null) :
      key === 'size' ? (typeof value === 'number' && Number.isSafeInteger(value) && value >= 0 ? String(value) : null) :
      typeof value === 'string' && value.length > 0 && value.length <= 180 && !/[\\/\x00-\x1f\x7f]/.test(value) ? value : null
    return safe ? [{ key, name: fieldLabels[key], value: safe }] : []
  })
}
export function summary(event: AuditEvent) {
  const detail = safeDetails(event)
  const get = (key: string) => detail.find(d => d.key === key)?.value
  const file = get('filename')
  const title = label(event.event_type) + (file && ['file_upload_completed', 'file_download_completed'].includes(event.event_type) ? ` ${file}` : '')
  const subtitle = event.event_type.endsWith('_failed')
    ? [file, get('reason')].filter(Boolean).join(' · ')
    : event.event_type.startsWith('agent_update_') && get('from_version') && get('target_version')
      ? `${get('from_version')} → ${get('target_version')}` : get('label') || get('name') || undefined
  return { title, subtitle }
}
