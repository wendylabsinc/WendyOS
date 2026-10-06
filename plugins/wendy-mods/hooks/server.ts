import type { McpToolResult } from 'claude-code'

import { isObj, leadingJson, str } from './json'
import type { Obj } from './json'

/** `$.mcp.call`, handed in so this module needs no engine to test. */
export type McpCall = (server: string, tool: string, args: Record<string, unknown>) => Promise<McpToolResult>

export type CallResult = { readonly ok: true; readonly value: Obj } | { readonly ok: false; readonly error: string }

/** Tried when no server is known: `wendy mcp setup`'s name, then the plugin's in tool-name spelling. */
export const CANDIDATES: readonly string[] = ['wendy', 'plugin_wendy_wendy']

/** The first CLI release whose `run` reports its target and which has `app_inspect`. */
export const MIN_CLI = '2026.09.30'

// Keep in step with the inline matchers in register.tsx, which the engine reads off the source.
const WENDY_TOOL = /^mcp__(.+)__(wendy_status|wendy_tools|device_list|device_connect|device_disconnect|device_info|container_list|telemetry_logs|app_inspect)$/
const RUN_TOOL = /^mcp__(.+)__run$/

export function serverFromTool(tool: string): string | undefined {
  return WENDY_TOOL.exec(tool)?.[1] ?? RUN_TOOL.exec(tool)?.[1]
}

/**
 * A tool result as a JSON object. Success results carry it in
 * `structuredContent` and as text; error results carry `{ error_code, message }`
 * and the text `[CODE] message` (go/internal/cli/mcp/errors.go:29).
 */
function textOf(result: McpToolResult): string {
  return result.content.map(block => (block.type === 'text' ? str(block.text) : '')).join('\n')
}

export function parseResult(result: McpToolResult): CallResult {
  const text = textOf(result)
  const structured = isObj(result.structuredContent) ? result.structuredContent : undefined
  if (result.isError) {
    const message = str(structured?.message)
    const code = str(structured?.error_code)
    if (message !== '') return { ok: false, error: code === '' ? message : `${code}: ${message}` }
    return { ok: false, error: text.trim() || 'the tool reported an error' }
  }
  const value = structured ?? leadingJson(text)?.value
  return value === undefined ? { ok: false, error: 'the tool returned no JSON object' } : { ok: true, value }
}

/** Text a successful result carries after its JSON: the CLI's one-time update notice (go/internal/cli/mcp/cli_update.go). */
export function noticeIn(result: McpToolResult): string {
  if (result.isError) return ''
  return leadingJson(textOf(result))?.rest.trim() ?? ''
}

/** Calls a Wendy tool; never rejects. */
export async function callWendy(call: McpCall, server: string, tool: string, args: Obj = {}): Promise<CallResult> {
  try {
    return parseResult(await call(server, tool, { ...args }))
  } catch (err) {
    return { ok: false, error: err instanceof Error ? err.message : String(err) }
  }
}

/** True when the server has no such tool, as with a Wendy CLI older than MIN_CLI. */
export function missingTool(error: string, tool: string): boolean {
  return error.includes(`no connected MCP tool "${tool}"`)
}

/** The first server that answers `wendy_status`: the override, then the candidates. */
export async function discover(call: McpCall, override: string): Promise<string | undefined> {
  const names = override === '' ? CANDIDATES : [override, ...CANDIDATES.filter(name => name !== override)]
  for (const name of names) {
    if ((await callWendy(call, name, 'wendy_status')).ok) return name
  }
  return undefined
}
