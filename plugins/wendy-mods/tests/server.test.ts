import type { McpToolResult } from 'claude-code'
import { describe, expect, test } from 'claude-code/testing'

import { callWendy, CANDIDATES, discover, missingTool, noticeIn, parseResult, serverFromTool } from '../hooks/server'
import type { McpCall } from '../hooks/server'
import { mcpError, mcpJson, mcpTextOnly, STATUS_CONNECTED } from './fixtures'

const missing = (server: string, tool: string) =>
  new Error(`wendy-mods: $.mcp.call: no connected MCP tool "${tool}" on a server named "${server}"`)

/** A fake $.mcp.call: the servers it knows answer wendy_status; it records every call. */
function fakeCall(known: readonly string[], calls: string[] = []): McpCall {
  return async (server, tool) => {
    calls.push(`${server}/${tool}`)
    if (!known.includes(server)) throw missing(server, tool)
    return mcpJson(STATUS_CONNECTED)
  }
}

describe('serverFromTool', () => {
  test('reads the server from Wendy tool names', () => {
    expect(serverFromTool('mcp__wendy__device_connect')).toBe('wendy')
    expect(serverFromTool('mcp__plugin_wendy_wendy__wendy_status')).toBe('plugin_wendy_wendy')
    expect(serverFromTool('mcp__plugin_wendy-spike_probe_root__device_list')).toBe('plugin_wendy-spike_probe_root')
    expect(serverFromTool('mcp__wendy__run')).toBe('wendy')
  })
  test('ignores tools that are not Wendy core tools', () => {
    expect(serverFromTool('mcp__plugin_linear_linear__list_issues')).toBeUndefined()
    expect(serverFromTool('Bash')).toBeUndefined()
  })
})

describe('parseResult', () => {
  test('prefers structuredContent', () => {
    expect(parseResult(mcpJson({ connected: true }))).toEqual({ ok: true, value: { connected: true } })
  })
  test('falls back to the JSON text', () => {
    expect(parseResult(mcpTextOnly({ connected: false }))).toEqual({ ok: true, value: { connected: false } })
  })
  test('reports an error result by code and message', () => {
    expect(parseResult(mcpError('NOT_CONNECTED', 'no device connection'))).toEqual({ ok: false, error: 'NOT_CONNECTED: no device connection' })
  })
  test('reports a text-only error result by its text', () => {
    const result: McpToolResult = { content: [{ type: 'text', text: '[TIMEOUT] gave up' }], isError: true }
    expect(parseResult(result)).toEqual({ ok: false, error: '[TIMEOUT] gave up' })
  })
  test('refuses text that is not a JSON object', () => {
    const result: McpToolResult = { content: [{ type: 'text', text: 'hello' }], isError: false }
    expect(parseResult(result)).toEqual({ ok: false, error: 'the tool returned no JSON object' })
  })
})

const NOTICE = 'A new Wendy CLI version is available: 2026.10.01-013419 (running 2026.09.30-213433).'

describe('the CLI update notice (go/internal/cli/mcp/cli_update.go)', () => {
  test('a result still parses when the notice follows as a second block', () => {
    const result: McpToolResult = { content: [{ type: 'text', text: '{"connected":true}' }, { type: 'text', text: NOTICE }], isError: false }
    expect(parseResult(result)).toEqual({ ok: true, value: { connected: true } })
    expect(noticeIn(result)).toBe(NOTICE)
  })
  test('a result still parses when the notice is joined into the text', () => {
    const result: McpToolResult = { content: [{ type: 'text', text: `{"connected":true}\n${NOTICE}` }], isError: false }
    expect(parseResult(result)).toEqual({ ok: true, value: { connected: true } })
    expect(noticeIn(result)).toBe(NOTICE)
  })
  test('a plain result has no notice', () => {
    expect(noticeIn(mcpJson({ connected: true }))).toBe('')
    expect(noticeIn(mcpError('NOT_CONNECTED', 'no device'))).toBe('')
  })
})

describe('callWendy', () => {
  test('passes server, tool and args through', async () => {
    const seen: unknown[] = []
    const call: McpCall = async (server, tool, args) => {
      seen.push([server, tool, args])
      return mcpJson({ ok: 1 })
    }
    expect(await callWendy(call, 'wendy', 'app_inspect', { app_name: 'a' })).toEqual({ ok: true, value: { ok: 1 } })
    expect(seen).toEqual([['wendy', 'app_inspect', { app_name: 'a' }]])
  })
  test('turns a rejection into an error result', async () => {
    const result = await callWendy(fakeCall([]), 'nope', 'wendy_status')
    expect(result.ok).toBe(false)
    expect(!result.ok && missingTool(result.error, 'wendy_status')).toBe(true)
  })
})

describe('missingTool', () => {
  test('matches only the named tool', () => {
    const error = missing('wendy', 'app_inspect').message
    expect(missingTool(error, 'app_inspect')).toBe(true)
    expect(missingTool(error, 'wendy_status')).toBe(false)
  })
})

describe('discover', () => {
  test('tries wendy, then the plugin server', async () => {
    const calls: string[] = []
    expect(CANDIDATES).toEqual(['wendy', 'plugin_wendy_wendy'])
    expect(await discover(fakeCall(['plugin_wendy_wendy'], calls), '')).toBe('plugin_wendy_wendy')
    expect(calls).toEqual(['wendy/wendy_status', 'plugin_wendy_wendy/wendy_status'])
  })
  test('tries the override first', async () => {
    const calls: string[] = []
    expect(await discover(fakeCall(['wendy-dev', 'wendy'], calls), 'wendy-dev')).toBe('wendy-dev')
    expect(calls).toEqual(['wendy-dev/wendy_status'])
  })
  test('does not try an override twice', async () => {
    const calls: string[] = []
    expect(await discover(fakeCall([], calls), 'wendy')).toBeUndefined()
    expect(calls).toEqual(['wendy/wendy_status', 'plugin_wendy_wendy/wendy_status'])
  })
})
