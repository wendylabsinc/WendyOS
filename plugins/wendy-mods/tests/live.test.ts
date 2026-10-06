import { describe, expect, test } from 'claude-code/testing'

import { asTarget, connectedTarget, connectionOf, sameTarget } from '../hooks/target'
import { inspectionFromContainers, NO_INSPECT_REASON, readInspection, verdictOf } from '../hooks/verify'
import type { Phase } from '../hooks/verify'
import {
  LIVE_CONTAINERS,
  LIVE_INSPECT_CRASHED,
  LIVE_INSPECT_HEALTHY,
  LIVE_INSPECT_NO_PROBES,
  LIVE_INSPECT_READINESS_FAILED,
  LIVE_RUN,
  LIVE_STATUS,
} from './live-fixtures'

const kindOf = (value: object, phase: Phase = 'final') => {
  const inspection = readInspection(value as Record<string, unknown>)
  return inspection === undefined ? 'unreadable' : verdictOf(inspection, phase).kind
}

describe('real device output', () => {
  test('the run target is the connected target', () => {
    expect(sameTarget(asTarget((LIVE_RUN as Record<string, unknown>).target), connectedTarget(LIVE_STATUS as Record<string, unknown>))).toBe(true)
  })
  test('the band shows the device and transport the server reports', () => {
    expect(connectionOf(LIVE_STATUS as Record<string, unknown>)).toEqual({
      device: 'fe80::720f:67c5:9653:4450%en11',
      transport: 'direct',
      target: { device: '[fe80::720f:67c5:9653:4450%en11]:50052', transport: 'direct' },
    })
  })
  test('verdicts match what the apps did', () => {
    expect(kindOf(LIVE_INSPECT_HEALTHY)).toBe('healthy')
    expect(kindOf(LIVE_INSPECT_NO_PROBES)).toBe('readiness-unknown')
    expect(kindOf(LIVE_INSPECT_CRASHED)).toBe('failing')
  })
  test('a refused probe is pending at the first check and failing after the window', () => {
    expect(kindOf(LIVE_INSPECT_READINESS_FAILED, 'initial')).toBe('readiness-unknown')
    expect(kindOf(LIVE_INSPECT_READINESS_FAILED, 'final')).toBe('failing')
  })
  test('crash-loop log lines are read', () => {
    const inspection = readInspection(LIVE_INSPECT_CRASHED as Record<string, unknown>)
    expect(inspection?.errors).toEqual(['fatal: simulated crash', 'fatal: simulated crash', 'fatal: simulated crash'])
    expect(inspection?.exit).toEqual({ code: 1, reason: 'crashed' })
  })
})

// app_inspect is listed only with the observability group on; container_list is core.
describe('container_list fallback on real output', () => {
  test('reads a crashed app', () => {
    const read = inspectionFromContainers(LIVE_CONTAINERS, 'sh.wendy.wendymods.crash')
    expect(read).toEqual({
      runningState: 'STOPPED',
      failureCount: 0,
      exit: { code: 1, reason: 'crashed' },
      servicesDown: [],
      readiness: 'unknown',
      readinessReason: NO_INSPECT_REASON,
      passedChecks: 0,
      errors: [],
    })
    expect(read === undefined ? undefined : verdictOf(read)).toEqual({ kind: 'failing', label: 'stopped (exit 1)' })
  })
  test('a running app has unknown readiness and says how to check it', () => {
    const read = inspectionFromContainers(LIVE_CONTAINERS, 'sh.wendy.wendymods.healthy')
    expect(read === undefined ? undefined : verdictOf(read, 'initial')).toEqual({ kind: 'readiness-unknown', label: 'readiness unknown' })
    expect(NO_INSPECT_REASON).toContain('wendy_tools')
  })
  test('an app that is not on the device reads as nothing', () => {
    expect(inspectionFromContainers(LIVE_CONTAINERS, 'sh.wendy.absent')).toBeUndefined()
  })
})
