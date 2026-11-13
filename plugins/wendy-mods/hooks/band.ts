import type { Band, VerdictKind } from '../types'
import { sameTarget } from './target'

export type Part = { readonly text: string; readonly color?: string; readonly bold?: true; readonly dim?: true }

/** The CLI's palette (go/internal/cli/tui/theme.go). */
export const COLORS = {
  label: '#34d399', // Emerald400, ColorPrimary
  device: '#6ee7b7', // Emerald300, deviceStyleTUI
  healthy: '#10b981', // Emerald500
  caution: '#f59e0b', // Amber500
  failing: '#ef4444', // Red500
} as const

const MARKS: Readonly<Record<VerdictKind, string>> = { healthy: '✓', 'readiness-unknown': '?', failing: '✗', 'not-verified': '–' }

const TINTS: Readonly<Record<VerdictKind, string>> = {
  healthy: COLORS.healthy,
  'readiness-unknown': COLORS.caution,
  failing: COLORS.failing,
  'not-verified': COLORS.caution,
}

/** Below this many columns the band drops the transport and narrows its gaps. */
export const NARROW_COLUMNS = 60

export function shortApp(appId: string): string {
  const segments = appId.split('.').filter(segment => segment !== '')
  return segments[segments.length - 1] ?? appId
}

/** The band's row as styled parts; none when no Wendy server is known. */
export function bandParts(band: Band, columns: number): Part[] {
  if (band === null) return []
  const parts: Part[] = [{ text: '▍wendy', color: COLORS.label, bold: true }, { text: '  ' }]
  const { connection, verdict } = band
  if (connection === null) {
    parts.push({ text: 'not connected', dim: true })
    return parts
  }
  const narrow = columns < NARROW_COLUMNS
  parts.push({ text: connection.device, color: COLORS.device, bold: true })
  if (!narrow && connection.transport !== '') parts.push({ text: ` · ${connection.transport}`, dim: true })
  if (verdict !== null && sameTarget(verdict.target, connection.target)) {
    parts.push({ text: narrow ? ' ' : '   ' })
    parts.push({ text: `${MARKS[verdict.kind]} ${shortApp(verdict.app)} ${verdict.label}`, color: TINTS[verdict.kind] })
  }
  return parts
}
