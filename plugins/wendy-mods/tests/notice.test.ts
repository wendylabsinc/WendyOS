import { expect, mock, test } from 'claude-code/testing'

import { RUN_STARTED } from './fixtures'
import { standIn, START } from './world'

// Review I2: the mod's own calls share the model's MCP session, so the server's
// one-time CLI update notice can land on them instead of on the model's calls.
const NOTICE =
  'A new Wendy CLI version is available: 2026.10.01-013419 (running 2026.09.30-213433). Update with: brew upgrade wendy. Then restart the Wendy MCP server to load the new version and tools.'
const RELAYED = `wendy-mods: the Wendy MCP server sent this notice to a wendy-mods background check, so it is relayed here: ${NOTICE}`
const RUN = { tool: 'mcp__wendy__run', project_path: '/proj' } as const

test('a notice the mod received reaches the model on its next Wendy call, once', async ($, on) => {
  const clock = mock.clock(on)
  standIn(on, { notice: NOTICE })
  on('tool.call', { tool: 'mcp__wendy__device_list' }, () => ({ result: [], text: '{"devices":[]}' }) as never)
  await $.session.start(START)
  await clock.settle()
  expect((await $.tool.call({ tool: 'mcp__wendy__device_list' })).context).toEqual([RELAYED])
  expect((await $.tool.call({ tool: 'mcp__wendy__device_list' })).context).toBeUndefined()
})

test('a notice received during a run is relayed after the verdict', async ($, on) => {
  const clock = mock.clock(on)
  standIn(on, { notice: NOTICE })
  const pending = $.tool.call(RUN)
  await clock.advance(3_000)
  const ran = await pending
  expect(ran.context?.length).toBe(2)
  expect(ran.context?.[0]).toContain('sh.wendy.demo is RUNNING on')
  expect(ran.context?.[1]).toBe(RELAYED)
})

test('a run result that carries the notice is still verified', async ($, on) => {
  const clock = mock.clock(on)
  standIn(on, { run: { kind: 'result', json: RUN_STARTED, suffix: `\n${NOTICE}` } })
  const pending = $.tool.call(RUN)
  await clock.advance(3_000)
  const ran = await pending
  expect(ran.context?.[0]).toContain('sh.wendy.demo is RUNNING on')
})
