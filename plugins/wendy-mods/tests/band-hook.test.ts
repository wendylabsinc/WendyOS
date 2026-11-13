import { expect, mock, test } from 'claude-code/testing'

import { STATUS_DISCONNECTED } from './fixtures'
import { BAND, ENGINE_BAND, standIn, START, SURFACES } from './world'

test('session start finds the wendy server and draws the device', async ($, on) => {
  const clock = mock.clock(on)
  const world = standIn(on)
  await $.session.start(START)
  await clock.settle()
  expect(world.calls[0]).toBe('wendy/wendy_status')
  for (const surface of SURFACES) {
    const ui = await $.ui.mount({ plugin: 'wendy-mods', surface, ...BAND })
    expect((await ui.find({ type: 'Text', text: /^▍wendy$/ }))?.props.color).toBe('#34d399')
    expect((await ui.find({ type: 'Text', text: /hopeful-glider\.local/ }))?.props.color).toBe('#6ee7b7')
    expect(await ui.find({ type: 'Text', text: / · lan/ })).toBeDefined()
    await ui.unmount()
  }
})

test('falls back to the plugin server name', async ($, on) => {
  const clock = mock.clock(on)
  const world = standIn(on, { servers: ['plugin_wendy_wendy'] })
  await $.session.start(START)
  await clock.settle()
  expect(world.calls.slice(0, 2)).toEqual(['wendy/wendy_status', 'plugin_wendy_wendy/wendy_status'])
  const ui = await $.ui.mount({ plugin: 'wendy-mods', surface: 'terminal', ...BAND })
  expect(await ui.find({ type: 'Text', text: /hopeful-glider/ })).toBeDefined()
})

test('tries the configured server first', { options: { server: 'wendy-dev' } }, async ($, on) => {
  const clock = mock.clock(on)
  const world = standIn(on, { servers: ['wendy-dev', 'wendy'] })
  await $.session.start(START)
  await clock.settle()
  expect(world.calls[0]).toBe('wendy-dev/wendy_status')
})

test('draws nothing when no server answers', async ($, on) => {
  const clock = mock.clock(on)
  standIn(on, { servers: [] })
  await $.session.start(START)
  await clock.settle()
  const ui = await $.ui.mount({ plugin: 'wendy-mods', surface: 'terminal', ...BAND })
  expect(await ui.find({ type: 'Text', text: /wendy/ })).toBeUndefined()
  expect(await ui.find({ type: 'Text', text: ENGINE_BAND })).toBeDefined()
})

test('says not connected', async ($, on) => {
  const clock = mock.clock(on)
  standIn(on, { status: STATUS_DISCONNECTED })
  await $.session.start(START)
  await clock.settle()
  const ui = await $.ui.mount({ plugin: 'wendy-mods', surface: 'terminal', ...BAND })
  expect((await ui.find({ type: 'Text', text: /not connected/ }))?.props.dimColor).toBe(true)
})

test('yields to a survey', async ($, on) => {
  const clock = mock.clock(on)
  standIn(on)
  await $.session.start(START)
  await clock.settle()
  const ui = await $.ui.mount({ plugin: 'wendy-mods', surface: 'terminal', ...BAND, props: { ...BAND.props, hasSurvey: true } })
  expect(await ui.find({ type: 'Text', text: /wendy/ })).toBeUndefined()
  expect(await ui.find({ type: 'Text', text: ENGINE_BAND })).toBeDefined()
})

test('learns the server from the model’s Wendy tool calls', async ($, on) => {
  const clock = mock.clock(on)
  standIn(on, { servers: ['wendy-dev'] })
  // A regex and a cast: the engine types tool names from the servers connected when it last loaded the mod.
  on('tool.call', { tool: /^mcp__wendy-dev__device_connect$/ }, () => ({ result: [], text: '{"connected":true}' }) as never)
  await $.session.start(START)
  await clock.settle()
  const before = await $.ui.mount({ plugin: 'wendy-mods', surface: 'terminal', ...BAND })
  expect(await before.find({ type: 'Text', text: /hopeful-glider/ })).toBeUndefined()
  await before.unmount()
  await $.tool.call({ tool: 'mcp__wendy-dev__device_connect', device: 'hopeful-glider.local' } as never)
  await clock.settle()
  const after = await $.ui.mount({ plugin: 'wendy-mods', surface: 'terminal', ...BAND })
  expect(await after.find({ type: 'Text', text: /hopeful-glider/ })).toBeDefined()
})

// Review Focus 4: the server restarts or vanishes mid-session.
test('clears after three failed polls and comes back on its own', async ($, on) => {
  const clock = mock.clock(on)
  const world = standIn(on)
  await $.session.start(START)
  await clock.settle()
  world.servers = []
  await clock.advance(30_000)
  await clock.advance(30_000)
  const still = await $.ui.mount({ plugin: 'wendy-mods', surface: 'terminal', ...BAND })
  expect(await still.find({ type: 'Text', text: /hopeful-glider/ })).toBeDefined()
  await still.unmount()
  await clock.advance(30_000)
  const gone = await $.ui.mount({ plugin: 'wendy-mods', surface: 'terminal', ...BAND })
  expect(await gone.find({ type: 'Text', text: /wendy/ })).toBeUndefined()
  await gone.unmount()
  world.servers = ['wendy']
  await clock.advance(30_000)
  const back = await $.ui.mount({ plugin: 'wendy-mods', surface: 'terminal', ...BAND })
  expect(await back.find({ type: 'Text', text: /hopeful-glider/ })).toBeDefined()
})
