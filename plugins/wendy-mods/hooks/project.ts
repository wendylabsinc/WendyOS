import { num, obj, parseJson, str } from './json'

const ABSOLUTE = /^(\/|[A-Za-z]:[\\/]|\\\\)/

/** `<project>/wendy.json`; a relative project path resolves against the session's directory. */
export function wendyJsonPath(cwd: string, projectPath: string): string {
  const joined = ABSOLUTE.test(projectPath) ? projectPath : `${cwd.replace(/[\\/]+$/, '')}/${projectPath}`
  const tidy = joined.replace(/\/\.(?=\/|$)/g, '').replace(/[\\/]+$/, '')
  return `${tidy}/wendy.json`
}

/** `wendy run`'s readiness timeout when it waits attached (commands/run.go:2833). */
const DEFAULT_READINESS_SECONDS = 30

/** Seconds Wendy allows a declared TCP readiness probe to pass; 0 when none is declared. */
export function readinessWindowFrom(text: string): number {
  const readiness = obj(parseJson(text)?.readiness)
  if (obj(readiness?.tcpSocket) === undefined) return 0
  const seconds = num(readiness?.timeoutSeconds) ?? 0
  return seconds > 0 ? seconds : DEFAULT_READINESS_SECONDS
}

/** The `appId` of a wendy.json text; '' when absent or unreadable. */
export function appIdFrom(text: string): string {
  return str(parseJson(text)?.appId)
}
