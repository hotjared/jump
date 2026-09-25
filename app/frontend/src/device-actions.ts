export type DeviceDeletionState = {
  hostname: string
  display_name: string | null
  online: boolean
  identity_state: 'active' | 'revoked' | 'none'
}

export const deviceDeleteMethod = 'DELETE'

export function deletionName(device: DeviceDeletionState): string {
  return device.display_name || device.hostname
}

export function canDeleteDevice(device: DeviceDeletionState): boolean {
  return device.identity_state === 'revoked'
}

export function deletionConfirmed(device: DeviceDeletionState, value: string | null): boolean {
  return value === deletionName(device)
}
