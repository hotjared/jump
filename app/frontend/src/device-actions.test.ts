import { describe, expect, it } from 'vitest'
import { canDeleteDevice, deletionConfirmed, deletionName, deviceDeleteMethod } from './device-actions'

describe('device deletion controls', () => {
  const revoked = {
    hostname: 'server01',
    display_name: 'Production DB',
    online: false,
    identity_state: 'revoked' as const,
  }

  it('only exposes deletion for revoked identities', () => {
    expect(canDeleteDevice(revoked)).toBe(true)
    expect(canDeleteDevice({ ...revoked, identity_state: 'active' })).toBe(false)
    expect(canDeleteDevice({ ...revoked, identity_state: 'none' })).toBe(false)
  })

  it('requires the exact displayed device name for confirmation', () => {
    expect(deletionName(revoked)).toBe('Production DB')
    expect(deletionConfirmed(revoked, 'Production DB')).toBe(true)
    expect(deletionConfirmed(revoked, 'production db')).toBe(false)
    expect(deletionConfirmed(revoked, null)).toBe(false)
  })

  it('uses DELETE for permanent removal', () => {
    expect(deviceDeleteMethod).toBe('DELETE')
  })
})
