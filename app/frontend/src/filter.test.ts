import { describe, expect, it } from 'vitest'
import { filterDevices } from './App'

const devices = [
  { id: 'a', hostname: 'prod-win', display_name: 'Domain Controller', current_user: 'Admin',
    primary_ip: '192.0.2.1', online: true, os_family: 'windows',
    group: { id: 'production', name: 'Production' }, tags: [{ id: 'critical', name: 'Critical' }] },
  { id: 'b', hostname: 'lab-linux', display_name: null, current_user: null, primary_ip: null,
    online: false, os_family: 'linux', group: null, tags: [] },
] as Parameters<typeof filterDevices>[0]

describe('device filters', () => {
  it('combines text, presence, operating system, group and tag', () => {
    expect(filterDevices(devices, 'controller', 'online', 'windows', 'production', 'critical').map(d => d.id)).toEqual(['a'])
    expect(filterDevices(devices, 'LAB', 'offline', 'linux', 'all', 'all').map(d => d.id)).toEqual(['b'])
  })
  it('excludes mismatched filters and handles missing metadata', () => {
    expect(filterDevices(devices, '', 'offline', 'windows', 'all', 'all')).toEqual([])
    expect(filterDevices(devices, '192.0.2.1', 'all', 'all', 'all', 'all')).toHaveLength(1)
  })
})
