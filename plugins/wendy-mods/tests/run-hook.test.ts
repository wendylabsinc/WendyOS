import { expect, mock, test } from 'claude-code/testing'
import type { Engine, MockClock } from 'claude-code/testing'

import {
  CONTAINERS_CRASHED,
  INSPECT_CRASH_LOOPING,
  INSPECT_NO_PROBES,
  RUN_CREATED,
  RUN_FAILED,
  RUN_NO_TARGET,
  STATUS_DISCONNECTED,
  STATUS_OTHER_DEVICE,
  WENDY_JSON,
} from './fixtures'
import { BAND, standIn, SURFACES } from './world'

const RUN = { tool: 'mcp__wendy__run', project_path: '/proj' } as const
const DEVICE = 'hopeful-glider.local:50051'

async function deploy($: Engine, clock: MockClock, input: Record<string, unknown> = RUN) {
  const pending = $.tool.call(input as never)
  await clock.advance(3_000)
  return pending
}

const inspects = (calls: readonly string[]) => calls.filter(call => call.endsWith('/app_inspect'))

test('a healthy deploy gets a healthy verdict in context and on the band', async ($, on) => {
  const clock = mock.clock(on)
  const world = standIn(on)
  const ran = await deploy($, clock)
  expect(ran.context).toEqual([
    `wendy-mods: sh.wendy.demo is RUNNING on ${DEVICE}, all services running, declared readiness checks passed (1 TCP). This covers TCP connectivity only.`,
  ])
  expect(world.inspectArgs).toEqual([{ app_name: 'sh.wendy.demo', project_path: '/proj', timeout_seconds: 5, max_logs: 5 }])
  expect(world.toasts).toEqual([])
  await clock.settle()
  for (const surface of SURFACES) {
    const ui = await $.ui.mount({ plugin: 'wendy-mods', surface, ...BAND })
    expect((await ui.find({ type: 'Text', text: /✓ demo healthy/ }))?.props.color).toBe('#10b981')
    await ui.unmount()
  }
})

test('a crash-looping deploy is flagged, toasted and drawn red', async ($, on) => {
  const clock = mock.clock(on)
  const world = standIn(on, { inspect: INSPECT_CRASH_LOOPING })
  const ran = await deploy($, clock)
  expect(ran.context?.[0]).toContain(`wendy-mods: deploy NOT healthy — sh.wendy.demo on ${DEVICE} is CRASH_LOOPING (failures 3, last exit 137 OOMKilled).`)
  expect(world.toasts).toEqual([`wendy: sh.wendy.demo crash-looping on ${DEVICE}`])
  await clock.settle()
  const ui = await $.ui.mount({ plugin: 'wendy-mods', surface: 'terminal', ...BAND })
  expect((await ui.find({ type: 'Text', text: /✗ demo crash-looping/ }))?.props.color).toBe('#ef4444')
})

test('no probes means readiness unknown', async ($, on) => {
  const clock = mock.clock(on)
  standIn(on, { inspect: INSPECT_NO_PROBES })
  const ran = await deploy($, clock)
  expect(ran.context?.[0]).toContain('3 s after deploy, but readiness is unknown (project declares no TCP readiness probes)')
})

test('failed, created and denied runs pass through untouched', async ($, on) => {
  const clock = mock.clock(on)
  const world = standIn(on, { run: { kind: 'result', json: RUN_FAILED, isError: true } })
  const failed = await deploy($, clock)
  expect(failed.isError).toBe(true)
  expect(failed.context).toBeUndefined()
  expect(failed.text).toBe(JSON.stringify(RUN_FAILED))
  world.run = { kind: 'result', json: RUN_CREATED }
  expect((await deploy($, clock)).context).toBeUndefined()
  world.run = { kind: 'deny', reason: 'not allowed here' }
  expect((await deploy($, clock)).deny).toBe('not allowed here')
  expect(inspects(world.calls)).toEqual([])
})

test('a deploy to another device than the connected one is not verified', async ($, on) => {
  const clock = mock.clock(on)
  const world = standIn(on, { status: STATUS_OTHER_DEVICE })
  const ran = await deploy($, clock)
  expect(ran.context).toEqual([
    `wendy-mods: could not verify this deploy — this session is connected to other-pi.local:50051, not to the deployed ${DEVICE}. Treat readiness as unknown.`,
  ])
  expect(inspects(world.calls)).toEqual([])
})

test('a session with no device is not verified', async ($, on) => {
  const clock = mock.clock(on)
  standIn(on, { status: STATUS_DISCONNECTED })
  const ran = await deploy($, clock)
  expect(ran.context?.[0]).toContain('could not verify this deploy — this session is not connected to a device')
})

test('a project without wendy.json is not verified', async ($, on) => {
  const clock = mock.clock(on)
  standIn(on, { files: {} })
  const ran = await deploy($, clock)
  expect(ran.context?.[0]).toContain('could not verify this deploy — no wendy.json at /proj/wendy.json')
})

test('an older CLI without app_inspect is named', async ($, on) => {
  const clock = mock.clock(on)
  standIn(on, { inspect: 'missing', containers: 'missing' })
  const ran = await deploy($, clock)
  expect(ran.context?.[0]).toContain('app_inspect is unavailable (needs Wendy CLI ≥ 2026.09.30)')
})

test('a run result without a target is named', async ($, on) => {
  const clock = mock.clock(on)
  standIn(on, { run: { kind: 'result', json: RUN_NO_TARGET } })
  const ran = await deploy($, clock)
  expect(ran.context?.[0]).toContain('the run result names no target (needs Wendy CLI ≥ 2026.09.30)')
})

test('verifies on the server the run went to', async ($, on) => {
  const clock = mock.clock(on)
  const world = standIn(on, { servers: ['plugin_wendy_wendy'] })
  await deploy($, clock, { tool: 'mcp__plugin_wendy_wendy__run', project_path: '/proj' })
  expect(inspects(world.calls)).toEqual(['plugin_wendy_wendy/app_inspect'])
})

// Review Focus 1: relative project paths.
test('a relative project path reads wendy.json from the session directory and is passed on verbatim', async ($, on) => {
  const clock = mock.clock(on)
  const world = standIn(on)
  const ran = await deploy($, clock, { tool: 'mcp__wendy__run', project_path: '.' })
  expect(ran.context?.[0]).toContain('is RUNNING on')
  expect(world.inspectArgs).toEqual([{ app_name: 'sh.wendy.demo', project_path: '.', timeout_seconds: 5, max_logs: 5 }])
})

// Review Focus 5: BOM and trailing slash.
test('a BOM in wendy.json and a trailing slash still find the app', async ($, on) => {
  const clock = mock.clock(on)
  const world = standIn(on, { files: { '/proj/app/wendy.json': `\uFEFF${WENDY_JSON}` } })
  const ran = await deploy($, clock, { tool: 'mcp__wendy__run', project_path: '/proj/app/' })
  expect(ran.context?.[0]).toContain('sh.wendy.demo is RUNNING')
  expect(inspects(world.calls)).toEqual(['wendy/app_inspect'])
})

// Live finding: $.mcp.call refuses tools the server does not list, and app_inspect is
// listed only with the observability group on. container_list is a core tool.
test('without app_inspect listed, a running app is checked through container_list', async ($, on) => {
  const clock = mock.clock(on)
  const world = standIn(on, { inspect: 'missing' })
  const ran = await deploy($, clock)
  expect(world.calls).toContain('wendy/container_list')
  expect(ran.context?.[0]).toContain(
    'sh.wendy.demo is RUNNING on hopeful-glider.local:50051 3 s after deploy, but readiness is unknown (app_inspect is not listed; add the "observability" tools group with wendy_tools so wendy-mods can check readiness).',
  )
})

test('without app_inspect listed, a crashed app is still flagged', async ($, on) => {
  const clock = mock.clock(on)
  const world = standIn(on, { inspect: 'missing', containers: CONTAINERS_CRASHED })
  const ran = await deploy($, clock)
  expect(ran.context?.[0]).toContain(`wendy-mods: deploy NOT healthy — sh.wendy.demo on ${DEVICE} is STOPPED (failures 0, last exit 1 crashed).`)
  expect(world.toasts).toEqual([`wendy: sh.wendy.demo stopped (exit 1) on ${DEVICE}`])
})

test('without app_inspect listed, an app missing from the device is not verified', async ($, on) => {
  const clock = mock.clock(on)
  standIn(on, { inspect: 'missing', containers: { containers: [] } })
  const ran = await deploy($, clock)
  expect(ran.context?.[0]).toContain("could not verify this deploy — sh.wendy.demo is not in the device's container list")
})
