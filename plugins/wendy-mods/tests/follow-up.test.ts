import { expect, mock, test } from 'claude-code/testing'

import { CONTAINERS_CRASHED, INSPECT_CRASH_LOOPING, INSPECT_HEALTHY, INSPECT_NO_PROBES, INSPECT_READINESS_FAILED, INSPECT_RECOVERED, STATUS_OTHER_DEVICE } from './fixtures'
import { BAND, standIn } from './world'

const RUN = { tool: 'mcp__wendy__run', project_path: '/proj' } as const
const DEVICE = 'hopeful-glider.local:50051'
const inspects = (calls: readonly string[]) => calls.filter(call => call.endsWith('/app_inspect')).length

test('a follow-up that finds a crash loop toasts and turns the band red', async ($, on) => {
  const clock = mock.clock(on)
  const world = standIn(on, { inspect: INSPECT_NO_PROBES })
  const pending = $.tool.call(RUN)
  await clock.advance(3_000)
  await pending
  world.inspect = INSPECT_CRASH_LOOPING
  await clock.advance(20_000)
  expect(inspects(world.calls)).toBe(2)
  expect(world.toasts).toEqual([`wendy: sh.wendy.demo crash-looping on ${DEVICE}`])
  const ui = await $.ui.mount({ plugin: 'wendy-mods', surface: 'terminal', ...BAND })
  expect(await ui.find({ type: 'Text', text: /✗ demo crash-looping/ })).toBeDefined()
})

// Review Focus 2: crashed and restarted between the checks.
test('a restart between the checks is still reported', async ($, on) => {
  const clock = mock.clock(on)
  const world = standIn(on, { inspect: INSPECT_NO_PROBES })
  const pending = $.tool.call(RUN)
  await clock.advance(3_000)
  await pending
  world.inspect = INSPECT_RECOVERED
  await clock.advance(20_000)
  expect(world.toasts).toEqual([`wendy: sh.wendy.demo restarted since deploy (failures 0 → 2) on ${DEVICE}`])
})

test('a follow-up that finds nothing worse stays quiet', async ($, on) => {
  const clock = mock.clock(on)
  const world = standIn(on, { inspect: INSPECT_NO_PROBES })
  const pending = $.tool.call(RUN)
  await clock.advance(3_000)
  await pending
  await clock.advance(20_000)
  expect(inspects(world.calls)).toBe(2)
  expect(world.toasts).toEqual([])
})

test('a failing first check schedules no follow-up', async ($, on) => {
  const clock = mock.clock(on)
  const world = standIn(on, { inspect: INSPECT_CRASH_LOOPING })
  const pending = $.tool.call(RUN)
  await clock.advance(3_000)
  await pending
  await clock.advance(20_000)
  expect(inspects(world.calls)).toBe(1)
})

test('a second run of the same app replaces the pending follow-up', async ($, on) => {
  const clock = mock.clock(on)
  const world = standIn(on, { inspect: INSPECT_NO_PROBES })
  const first = $.tool.call(RUN)
  await clock.advance(3_000)
  await first
  const second = $.tool.call(RUN)
  await clock.advance(3_000)
  await second
  expect(inspects(world.calls)).toBe(2)
  await clock.advance(17_000) // t = 23 s: the first follow-up would have fired
  expect(inspects(world.calls)).toBe(2)
  await clock.advance(3_000) // t = 26 s: the second one fires
  expect(inspects(world.calls)).toBe(3)
})

test('a follow-up skips when the session moved to another device', async ($, on) => {
  const clock = mock.clock(on)
  const world = standIn(on, { inspect: INSPECT_NO_PROBES })
  const pending = $.tool.call(RUN)
  await clock.advance(3_000)
  await pending
  world.status = STATUS_OTHER_DEVICE
  world.inspect = INSPECT_CRASH_LOOPING
  await clock.advance(20_000)
  expect(inspects(world.calls)).toBe(1)
  expect(world.toasts).toEqual([])
})

// Review I1: a slow-starting app is not failing until Wendy's readiness window ends.
test('a slow-starting app is rechecked when the readiness window ends', async ($, on) => {
  const clock = mock.clock(on)
  const world = standIn(on, { inspect: INSPECT_READINESS_FAILED })
  const pending = $.tool.call(RUN)
  await clock.advance(3_000)
  const ran = await pending
  expect(ran.context?.[0]).toContain('is not passing yet')
  expect(world.toasts).toEqual([])
  world.inspect = INSPECT_HEALTHY
  await clock.advance(20_000) // t = 23 s: inside WENDY_JSON's default 30 s window
  expect(inspects(world.calls)).toBe(1)
  await clock.advance(10_000) // t = 33 s: the window has ended
  expect(inspects(world.calls)).toBe(2)
  expect(world.toasts).toEqual([])
  const ui = await $.ui.mount({ plugin: 'wendy-mods', surface: 'terminal', ...BAND })
  expect(await ui.find({ type: 'Text', text: /✓ demo healthy/ })).toBeDefined()
})

test('readiness still failing when the window ends is reported', async ($, on) => {
  const clock = mock.clock(on)
  const world = standIn(on, { inspect: INSPECT_READINESS_FAILED })
  const pending = $.tool.call(RUN)
  await clock.advance(3_000)
  await pending
  await clock.advance(30_000)
  expect(world.toasts).toEqual([`wendy: sh.wendy.demo readiness failed on ${DEVICE}`])
})

test('without app_inspect listed, the follow-up still catches a crash', async ($, on) => {
  const clock = mock.clock(on)
  const world = standIn(on, { inspect: 'missing' })
  const pending = $.tool.call(RUN)
  await clock.advance(3_000)
  await pending
  world.containers = CONTAINERS_CRASHED
  await clock.advance(20_000)
  expect(world.toasts).toEqual([`wendy: sh.wendy.demo stopped (exit 1) on ${DEVICE}`])
})
