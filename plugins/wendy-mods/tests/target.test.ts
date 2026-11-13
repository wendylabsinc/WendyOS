import { describe, expect, test } from 'claude-code/testing'

import { asTarget, connectedTarget, connectionOf, sameTarget, targetKey } from '../hooks/target'
import { STATUS_CONNECTED, STATUS_DISCONNECTED } from './fixtures'

const lan = (device: string) => ({ device, transport: 'lan' })

describe('targetKey', () => {
  test('a bare host gains the default agent port', () => {
    expect(targetKey(lan('hopeful-glider.local'))).toBe('addr:hopeful-glider.local:50051')
  })
  test('host:port is kept', () => {
    expect(targetKey(lan('10.0.0.5:50052'))).toBe('addr:10.0.0.5:50052')
  })
  test('host names ignore case', () => {
    expect(targetKey(lan('Hopeful-Glider.LOCAL'))).toBe('addr:hopeful-glider.local:50051')
  })
  test('a bare IPv6 address is bracketed', () => {
    expect(targetKey(lan('fe80::1'))).toBe('addr:[fe80::1]:50051')
    expect(targetKey(lan('[fe80::1]'))).toBe('addr:[fe80::1]:50051')
    expect(targetKey(lan('[fe80::1]:50051'))).toBe('addr:[fe80::1]:50051')
  })
  test('vm and cloud selectors stand for themselves', () => {
    expect(targetKey({ device: 'vm:sim1', transport: 'vm' })).toBe('vm:sim1')
    expect(targetKey({ device: 'cloud://org/device', transport: 'cloud' })).toBe('cloud://org/device')
  })
  test('a cloud device name carries its endpoint', () => {
    expect(targetKey({ device: 'hopeful-glider', transport: 'cloud', cloud_grpc: 'cloud.wendy.dev:443' })).toBe('name:hopeful-glider@cloud.wendy.dev:443')
  })
  test('a selector wins over the device', () => {
    expect(targetKey({ device: 'hopeful-glider', transport: 'cloud', selector: 'cloud://t/o/a' })).toBe('cloud://t/o/a')
  })
  test('an empty device has no key', () => {
    expect(targetKey(lan(''))).toBeUndefined()
  })
})

describe('sameTarget', () => {
  test('a bare host equals host:50051', () => {
    expect(sameTarget(lan('pi.local'), lan('pi.local:50051'))).toBe(true)
  })
  test('letter case does not matter', () => {
    expect(sameTarget(lan('PI.local'), lan('pi.LOCAL:50051'))).toBe(true)
  })
  test('another port is another device', () => {
    expect(sameTarget(lan('pi.local:50052'), lan('pi.local'))).toBe(false)
  })
  test('a hostname and an IP are not assumed equal', () => {
    expect(sameTarget(lan('pi.local'), lan('10.0.0.5'))).toBe(false)
  })
  test('cloud names match only on the same endpoint', () => {
    const a = { device: 'g', transport: 'cloud', cloud_grpc: 'cloud.wendy.dev:443' }
    expect(sameTarget(a, { ...a })).toBe(true)
    expect(sameTarget(a, { ...a, cloud_grpc: 'staging.wendy.dev:443' })).toBe(false)
  })
  test('a selector and a bare device are not assumed equal', () => {
    expect(sameTarget({ device: 'g', transport: 'cloud', selector: 'cloud://t/o/a' }, { device: 'g', transport: 'cloud' })).toBe(false)
  })
  test('a missing target is never the same', () => {
    expect(sameTarget(undefined, lan('pi.local'))).toBe(false)
    expect(sameTarget(lan('pi.local'), undefined)).toBe(false)
  })
})

describe('asTarget', () => {
  test('keeps only identity fields that are set', () => {
    expect(asTarget({ device: 'pi.local', transport: 'lan', broker_url: 'x' })).toEqual({ device: 'pi.local', transport: 'lan' })
    expect(asTarget({ device: 'g', transport: 'cloud', cloud_grpc: 'c:443', selector: 'cloud://a' })).toEqual({ device: 'g', transport: 'cloud', cloud_grpc: 'c:443', selector: 'cloud://a' })
  })
  test('rejects what names no device', () => {
    expect(asTarget({ transport: 'lan' })).toBeUndefined()
    expect(asTarget('pi.local')).toBeUndefined()
    expect(asTarget(undefined)).toBeUndefined()
  })
})

describe('connectedTarget and connectionOf', () => {
  test('prefer command_target', () => {
    expect(connectedTarget(STATUS_CONNECTED)).toEqual({ device: 'hopeful-glider.local:50051', transport: 'lan' })
    expect(connectionOf(STATUS_CONNECTED)).toEqual({
      device: 'hopeful-glider.local',
      transport: 'lan',
      target: { device: 'hopeful-glider.local:50051', transport: 'lan' },
    })
  })
  test('fall back to device and connection_type', () => {
    const { command_target: _, ...older } = STATUS_CONNECTED
    expect(connectedTarget(older)).toEqual({ device: 'hopeful-glider.local', transport: 'lan' })
  })
  test('not connected yields nothing', () => {
    expect(connectedTarget(STATUS_DISCONNECTED)).toBeUndefined()
    expect(connectionOf(STATUS_DISCONNECTED)).toBeNull()
  })
})
