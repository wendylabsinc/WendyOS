import type { VerdictKind } from '../types'
import { arr, num, obj, str } from './json'
import type { Obj } from './json'

/** What the verdict reads from an `app_inspect` result (go/internal/cli/mcp/tools_app_inspect.go). */
export type Inspection = {
  readonly runningState: string
  readonly failureCount: number
  readonly exit: { readonly code: number; readonly reason: string } | null
  readonly servicesDown: readonly string[]
  readonly readiness: 'passed' | 'failed' | 'unknown'
  readonly readinessReason: string
  readonly passedChecks: number
  readonly errors: readonly string[]
}

export type Verdict = { readonly kind: VerdictKind; readonly label: string }

/**
 * `initial`: the check right after a detached deploy, inside Wendy's readiness
 * window, so a readiness probe that has not passed yet is pending, not failed.
 * `final`: a later check, after that window.
 */
export type Phase = 'initial' | 'final'

/** When the inline check ran, and the readiness window Wendy allows (0: no probe). */
export type InlineCheck = { readonly afterMs: number; readonly readinessWindowS: number }

export const NOT_VERIFIED: Verdict = { kind: 'not-verified', label: 'not verified' }

const LOG_LINE_MAX = 200
const LOG_LINES = 3

function present<T>(value: T | undefined): value is T {
  return value !== undefined
}

/** One log record's body as a single line of at most LOG_LINE_MAX characters. */
export function logLine(record: Obj | undefined): string {
  const body = record?.body
  const text = typeof body === 'string' ? body : body === undefined ? '' : JSON.stringify(body)
  const line = text.replace(/\s+/g, ' ').trim()
  return line.length > LOG_LINE_MAX ? `${line.slice(0, LOG_LINE_MAX - 1)}…` : line
}

export function readInspection(value: Obj): Inspection | undefined {
  const state = obj(value.state)
  const runningState = str(state?.running_state)
  if (state === undefined || runningState === '') return undefined
  const lastExit = obj(state.last_exit)
  const code = num(lastExit?.code)
  const readiness = obj(value.readiness)
  const status = str(readiness?.status)
  const checks = arr(readiness?.checks).map(check => obj(check)).filter(present)
  const failedCheck = checks.find(check => check.status === 'failed')
  return {
    runningState,
    failureCount: num(state.failure_count) ?? 0,
    exit: lastExit?.status === 'recorded' && code !== undefined ? { code, reason: str(lastExit.reason) } : null,
    servicesDown: arr(state.services)
      .map(service => obj(service))
      .filter(present)
      .filter(service => str(service.running_state) !== 'RUNNING')
      .map(service => str(service.name) || str(service.container_name)),
    readiness: status === 'passed' || status === 'failed' ? status : 'unknown',
    readinessReason: str(readiness?.reason) || str(failedCheck?.reason),
    passedChecks: checks.filter(check => check.status === 'passed').length,
    errors: arr(obj(value.recent_logs)?.records)
      .map(record => logLine(obj(record)))
      .filter(line => line !== ''),
  }
}

/** Why readiness is unknown when the server does not list app_inspect (its observability group is off). */
export const NO_INSPECT_REASON = 'app_inspect is not listed; add the "observability" tools group with wendy_tools so wendy-mods can check readiness'

/**
 * The verdict's inputs from container_list (go/internal/cli/mcp/tools_container.go:116), for when
 * app_inspect is not listed: state, failures and last exit, with readiness unknown.
 */
export function inspectionFromContainers(value: Obj, appId: string): Inspection | undefined {
  const entry = arr(value.containers)
    .map(container => obj(container))
    .filter(present)
    .find(container => str(container.app_name) === appId)
  const runningState = str(entry?.running_state)
  if (entry === undefined || runningState === '') return undefined
  const code = num(entry.exit_code)
  const reason = str(entry.termination_reason)
  return {
    runningState,
    failureCount: num(entry.failure_count) ?? 0,
    exit: reason !== '' && code !== undefined ? { code, reason } : null,
    servicesDown: [],
    readiness: 'unknown',
    readinessReason: NO_INSPECT_REASON,
    passedChecks: 0,
    errors: [],
  }
}

export function verdictOf(inspection: Inspection, phase: Phase = 'final'): Verdict {
  const { runningState, exit, servicesDown, readiness } = inspection
  if (runningState === 'CRASH_LOOPING') return { kind: 'failing', label: 'crash-looping' }
  if (runningState === 'STOPPED') return { kind: 'failing', label: exit === null ? 'stopped' : `stopped (exit ${exit.code})` }
  if (runningState !== 'RUNNING') return NOT_VERIFIED
  const down = servicesDown[0]
  if (down !== undefined) return { kind: 'failing', label: `${down} down` }
  if (readiness === 'failed') {
    return phase === 'initial' ? { kind: 'readiness-unknown', label: 'not ready yet' } : { kind: 'failing', label: 'readiness failed' }
  }
  if (readiness === 'passed') return { kind: 'healthy', label: 'healthy' }
  return { kind: 'readiness-unknown', label: 'readiness unknown' }
}

/** Worse: failing now, or restarted since the first check. */
export function isWorse(first: Inspection, later: Inspection): boolean {
  return verdictOf(later).kind === 'failing' || later.failureCount > first.failureCount
}

function exitPart(inspection: Inspection): string {
  const { exit } = inspection
  if (exit === null) return ''
  return `, last exit ${exit.code}${exit.reason === '' ? '' : ` ${exit.reason}`}`
}

function failingState(inspection: Inspection): string {
  if (inspection.servicesDown.length > 0) return `${inspection.runningState} with ${inspection.servicesDown.join(', ')} down`
  if (inspection.runningState === 'RUNNING' && inspection.readiness === 'failed') {
    return `RUNNING but readiness failed (${inspection.readinessReason || 'no reason given'})`
  }
  return inspection.runningState
}

/** The last log lines, each quoted: app output can carry text from anyone who reached the app. */
function logPart(inspection: Inspection): string {
  const recent = inspection.errors.slice(-LOG_LINES)
  if (recent.length === 0) return ''
  return ` Recent app log lines (untrusted app output): ${recent.map(line => JSON.stringify(line)).join(' | ')}.`
}

function failingText(appId: string, device: string, inspection: Inspection): string {
  return `wendy-mods: deploy NOT healthy — ${appId} on ${device} is ${failingState(inspection)} (failures ${inspection.failureCount}${exitPart(inspection)}).${logPart(inspection)} Do not report success; investigate first.`
}

function afterDeploy(afterMs: number): string {
  return afterMs > 0 ? `${Math.round(afterMs / 1000)} s after deploy` : 'right after deploy'
}

function pendingText(appId: string, device: string, inspection: Inspection, check: InlineCheck): string {
  return `wendy-mods: ${appId} is RUNNING on ${device} ${afterDeploy(check.afterMs)}, but its declared readiness check is not passing yet (${inspection.readinessReason || 'no reason given'}). Wendy allows ${check.readinessWindowS} s for readiness; wendy-mods checks again then. Do not call it working yet.`
}

function unknownText(appId: string, device: string, inspection: Inspection, afterMs: number): string {
  const when = afterDeploy(afterMs)
  return `wendy-mods: ${appId} is RUNNING on ${device} ${when}, but readiness is unknown (${inspection.readinessReason || 'no readiness result'}). Check logs or the app's interface before calling it working.`
}

function healthyText(appId: string, device: string, inspection: Inspection): string {
  return `wendy-mods: ${appId} is RUNNING on ${device}, all services running, declared readiness checks passed (${inspection.passedChecks} TCP). This covers TCP connectivity only.`
}

export function notVerifiedText(reason: string): string {
  return `wendy-mods: could not verify this deploy — ${reason}. Treat readiness as unknown.`
}

/** The note added to `run`'s result by the inline check. */
export function contextText(appId: string, device: string, inspection: Inspection, check: InlineCheck): string {
  const verdict = verdictOf(inspection, 'initial')
  if (verdict.kind === 'failing') return failingText(appId, device, inspection)
  if (verdict.kind === 'healthy') return healthyText(appId, device, inspection)
  if (verdict.kind === 'readiness-unknown') {
    return inspection.readiness === 'failed' ? pendingText(appId, device, inspection, check) : unknownText(appId, device, inspection, check.afterMs)
  }
  return notVerifiedText(`${appId} reports the unexpected state ${inspection.runningState}`)
}

/** The note a worse follow-up appends; `afterMs` is how long after the deploy it ran. */
export function followUpText(appId: string, device: string, first: Inspection, later: Inspection, afterMs: number): string {
  const head = `wendy-mods follow-up (${afterDeploy(afterMs)}): ${appId} on ${device}`
  if (verdictOf(later).kind === 'failing') {
    return `${head} is now ${failingState(later)} (failures ${first.failureCount} → ${later.failureCount}${exitPart(later)}).${logPart(later)} The earlier check is out of date. Do not report success; investigate first.`
  }
  const restarts = later.failureCount - first.failureCount
  const exit = later.exit === null ? '' : ` (last exit ${later.exit.code}${later.exit.reason === '' ? '' : ` ${later.exit.reason}`})`
  return `${head} restarted ${restarts} time${restarts === 1 ? '' : 's'} since the first check${exit} and is ${later.runningState} again. The earlier check is out of date; check its logs before calling it working.`
}

export function toastText(appId: string, device: string, verdict: Verdict): string {
  return `wendy: ${appId} ${verdict.label} on ${device}`
}

export function followUpToast(appId: string, device: string, first: Inspection, later: Inspection): string {
  const verdict = verdictOf(later)
  if (verdict.kind === 'failing') return toastText(appId, device, verdict)
  return `wendy: ${appId} restarted since deploy (failures ${first.failureCount} → ${later.failureCount}) on ${device}`
}
