import { atom, read, update } from 'claude-code'
import type { EngineInterface, Register, Timer } from 'claude-code'

import type { Band, BandVerdict, WendyTarget } from '../types'
import { bandParts } from './band'
import type { Part } from './band'
import { leadingJson, str } from './json'
import { appIdFrom, readinessWindowFrom, wendyJsonPath } from './project'
import { callWendy, discover, MIN_CLI, missingTool, noticeIn, serverFromTool } from './server'
import type { McpCall } from './server'
import { asTarget, connectedTarget, connectionOf, sameTarget } from './target'
import { contextText, followUpText, followUpToast, inspectionFromContainers, isWorse, NOT_VERIFIED, notVerifiedText, readInspection, toastText, verdictOf } from './verify'
import type { Inspection } from './verify'

const band = atom({ plugin: 'wendy-mods', key: 'band' } as const, null as Band)

const POLL_MS = 30_000
const MAX_POLL_FAILURES = 3
const SETTLE_MS = 3_000
const SETTLE_MIN_BUDGET_MS = 6_000
const FOLLOW_UP_MS = 20_000
const INSPECT_ARGS = { timeout_seconds: 5, max_logs: 5 } as const

// The engine lets `$` reach only functions declared at the top of this file, so
// the helpers live here and share this state. `register` resets it; a hot reload
// starts it over, which the spec accepts.
let override = ''
let server: string | undefined
let pollFailures = 0
const followUps = new Map<string, Timer>()
// Notices the server sent to the mod's own calls, held for the model's next Wendy call.
let pendingNotices: string[] = []

function textProps(part: Part): { color?: string; bold?: true; dimColor?: true } {
  return {
    ...(part.color === undefined ? {} : { color: part.color }),
    ...(part.bold === true ? { bold: true as const } : {}),
    ...(part.dim === true ? { dimColor: true as const } : {}),
  }
}

function keepNotice(notice: string): void {
  if (notice !== '' && !pendingNotices.includes(notice)) pendingNotices.push(notice)
}

/** The held notices, worded for the model, and forgotten. */
function relayNotices(): string[] {
  const relayed = pendingNotices.map(notice => `wendy-mods: the Wendy MCP server sent this notice to a wendy-mods background check, so it is relayed here: ${notice}`)
  pendingNotices = []
  return relayed
}

// The mod's calls share the model's MCP session, so they can receive the
// server's one-time CLI update notice; keep it for the model.
const mcpOf =
  ($: EngineInterface): McpCall =>
  async (name, tool, args) => {
    const result = await $.mcp.call(name, tool, args)
    keepNotice(noticeIn(result))
    return result
  }

function useServer(name: string): void {
  if (name !== server) {
    server = name
    pollFailures = 0
  }
}

/** Re-reads the connection for the band; finds a server first when none is known. */
async function refresh($: EngineInterface): Promise<void> {
  if (server === undefined) {
    server = await discover(mcpOf($), override)
    if (server === undefined) {
      await update($, band, () => null)
      return
    }
  }
  const status = await callWendy(mcpOf($), server, 'wendy_status')
  if (!status.ok) {
    pollFailures += 1
    if (pollFailures >= MAX_POLL_FAILURES) {
      server = undefined
      pollFailures = 0
      await update($, band, () => null)
    }
    return
  }
  pollFailures = 0
  const connection = connectionOf(status.value)
  await update($, band, previous => ({ connection, verdict: previous?.verdict ?? null }))
}

/** Refreshes in a dispatch of its own, so the caller never waits on it. */
function refreshSoon($: EngineInterface): void {
  $.clock.after(0, () => {
    refresh($).catch(() => undefined)
  })
}

/** How a target is named to the model and in toasts. */
function nameOf(target: WendyTarget): string {
  return target.selector ?? target.device
}

async function setVerdict($: EngineInterface, verdict: BandVerdict): Promise<void> {
  await update($, band, previous => ({ connection: previous?.connection ?? null, verdict }))
}

type Inspected = { readonly ok: true; readonly inspection: Inspection } | { readonly ok: false; readonly reason: string }

/**
 * The app's state from app_inspect, or from container_list when the server does
 * not list app_inspect (its observability group is off): `$.mcp.call` only
 * reaches tools the server lists.
 */
async function inspectApp($: EngineInterface, runServer: string, appId: string, projectPath: string): Promise<Inspected> {
  const inspected = await callWendy(mcpOf($), runServer, 'app_inspect', { app_name: appId, project_path: projectPath, ...INSPECT_ARGS })
  if (inspected.ok) {
    const inspection = readInspection(inspected.value)
    return inspection === undefined ? { ok: false, reason: 'app_inspect returned no app state' } : { ok: true, inspection }
  }
  if (!missingTool(inspected.error, 'app_inspect')) return { ok: false, reason: `app_inspect failed: ${inspected.error}` }
  const listed = await callWendy(mcpOf($), runServer, 'container_list')
  if (!listed.ok) {
    return {
      ok: false,
      reason: missingTool(listed.error, 'container_list') ? `app_inspect is unavailable (needs Wendy CLI ≥ ${MIN_CLI})` : `container_list failed: ${listed.error}`,
    }
  }
  const inspection = inspectionFromContainers(listed.value, appId)
  return inspection === undefined ? { ok: false, reason: `${appId} is not in the device's container list` } : { ok: true, inspection }
}

/** Checks the app again later; speaks only when things got worse. */
async function followUp(
  $: EngineInterface,
  runServer: string,
  appId: string,
  projectPath: string,
  deployed: WendyTarget,
  first: Inspection,
  afterMs: number,
): Promise<void> {
  followUps.delete(appId)
  const status = await callWendy(mcpOf($), runServer, 'wendy_status')
  if (!status.ok || !sameTarget(deployed, connectedTarget(status.value))) return
  const again = await inspectApp($, runServer, appId, projectPath)
  if (!again.ok) return
  const later = again.inspection
  await setVerdict($, { app: appId, target: deployed, ...verdictOf(later) })
  if (!isWorse(first, later)) return
  $.ui.toast(followUpToast(appId, nameOf(deployed), first, later))
  await $.session.append({
    message: { type: 'user', content: [{ type: 'text', text: followUpText(appId, nameOf(deployed), first, later, afterMs) }] },
  })
}

function scheduleFollowUp(
  $: EngineInterface,
  runServer: string,
  appId: string,
  projectPath: string,
  deployed: WendyTarget,
  first: Inspection,
  delayMs: number,
  afterMs: number,
): void {
  followUps.get(appId)?.cancel()
  followUps.set(
    appId,
    $.clock.after(delayMs, () => {
      followUp($, runServer, appId, projectPath, deployed, first, afterMs).catch(() => undefined)
    }),
  )
}

/** The note for the model about a `run` that reported `started`; undefined for any other run. */
async function verifyRun(
  $: EngineInterface,
  tool: string,
  projectPath: string,
  runText: string,
  signal: AbortSignal,
  remainingMs: () => number,
): Promise<string | undefined> {
  const run = leadingJson(runText)?.value
  const runServer = serverFromTool(tool)
  if (run === undefined || run.status !== 'started' || runServer === undefined) return undefined
  useServer(runServer)
  refreshSoon($)

  const deployed = asTarget(run.target)
  if (deployed === undefined) return notVerifiedText(`the run result names no target (needs Wendy CLI ≥ ${MIN_CLI})`)
  const unverified = async (appId: string, reason: string): Promise<string> => {
    await setVerdict($, { app: appId === '' ? 'deploy' : appId, target: deployed, ...NOT_VERIFIED })
    return notVerifiedText(reason)
  }

  const status = await callWendy(mcpOf($), runServer, 'wendy_status')
  const connected = status.ok ? connectedTarget(status.value) : undefined
  if (connected === undefined) return unverified('', 'this session is not connected to a device')
  if (!sameTarget(deployed, connected)) {
    return unverified('', `this session is connected to ${nameOf(connected)}, not to the deployed ${nameOf(deployed)}`)
  }

  const file = wendyJsonPath(await $.session.cwd(), projectPath)
  let project: string
  try {
    project = String(await $.fs.read(file))
  } catch {
    return unverified('', `no wendy.json at ${file}`)
  }
  const appId = appIdFrom(project)
  if (appId === '') return unverified('', `no appId in ${file}`)
  const readinessWindowS = readinessWindowFrom(project)

  let settledMs = 0
  if (remainingMs() > SETTLE_MIN_BUDGET_MS) {
    await $.clock.sleep(SETTLE_MS, { signal })
    settledMs = SETTLE_MS
  }

  const inspected = await inspectApp($, runServer, appId, projectPath)
  if (!inspected.ok) return unverified(appId, inspected.reason)
  const inspection = inspected.inspection

  const verdict = verdictOf(inspection, 'initial')
  await setVerdict($, { app: appId, target: deployed, ...verdict })
  if (verdict.kind === 'failing') $.ui.toast(toastText(appId, nameOf(deployed), verdict))
  if (verdict.kind === 'healthy' || verdict.kind === 'readiness-unknown') {
    // A probe that has not passed yet is rechecked once Wendy's readiness window is over.
    const delayMs = inspection.readiness === 'failed' ? Math.max(FOLLOW_UP_MS, readinessWindowS * 1000) : FOLLOW_UP_MS
    scheduleFollowUp($, runServer, appId, projectPath, deployed, inspection, delayMs, settledMs + delayMs)
  }
  return contextText(appId, nameOf(deployed), inspection, { afterMs: settledMs, readinessWindowS })
}

export const register: Register = (on, options) => {
  override = typeof options.server === 'string' ? options.server.trim() : ''
  server = undefined
  pollFailures = 0
  followUps.clear()
  pendingNotices = []

  on('session.start', async ($, e, next) => {
    const started = await next(e)
    refreshSoon($)
    $.clock.every(POLL_MS, () => {
      refresh($).catch(() => undefined)
    })
    return started
  })

  // The model's own Wendy calls name the server whose connection it relies on.
  on(
    'tool.call',
    { tool: /^mcp__(.+)__(wendy_status|wendy_tools|device_list|device_connect|device_disconnect|device_info|container_list|telemetry_logs|app_inspect)$/ },
    async ($, e, next) => {
      const ran = await next(e)
      const seen = serverFromTool(e.tool)
      if (seen !== undefined) useServer(seen)
      refreshSoon($)
      if (ran.deny !== undefined || ran.isError === true || pendingNotices.length === 0) return ran
      return { ...ran, context: [...(ran.context ?? []), ...relayNotices()] }
    },
  )

  on('tool.call', { tool: /^mcp__(.+)__run$/ }, async ($, e, next) => {
    const ran = await next(e)
    if (ran.deny !== undefined || ran.isError === true) return ran
    try {
      const projectPath = str((e as Readonly<Record<string, unknown>>).project_path)
      const note = await verifyRun($, e.tool, projectPath, ran.text ?? '', next.signal, () => next.budget.remainingMs)
      const added = [...(note === undefined ? [] : [note]), ...relayNotices()]
      return added.length === 0 ? ran : { ...ran, context: [...(ran.context ?? []), ...added] }
    } catch {
      // An interrupt during the settle, or any bug here, must never cost the model the run result.
      return ran
    }
  })

  on('ui.render', { component: 'AbovePrompt' }, async ($, e, next) => {
    const value = await read($, band)
    if (value === null || e.props.hasSurvey) return next(e)
    const { Box, Text } = $.ui.resolve(e)
    return (
      <Box>
        {bandParts(value, e.props.bodyColumns).map(part => (
          <Text {...textProps(part)}>{part.text}</Text>
        ))}
      </Box>
    )
  })
}
