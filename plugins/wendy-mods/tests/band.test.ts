import { describe, expect, test } from 'claude-code/testing'

import { bandParts, COLORS, shortApp } from '../hooks/band'
import type { Band, BandVerdict } from '../types'

const TARGET = { device: 'hopeful-glider.local:50051', transport: 'lan' }
const CONNECTED: Band = { connection: { device: 'hopeful-glider.local', transport: 'lan', target: TARGET }, verdict: null }

const withVerdict = (verdict: Omit<BandVerdict, 'app' | 'target'>, target = TARGET): Band => ({
  connection: CONNECTED?.connection ?? null,
  verdict: { app: 'sh.wendy.demo', target, ...verdict },
})

const text = (band: Band, columns = 100) => bandParts(band, columns).map(part => part.text).join('')

describe('shortApp', () => {
  test('keeps the last dot-segment', () => {
    expect(shortApp('sh.wendy.demo')).toBe('demo')
    expect(shortApp('demo')).toBe('demo')
    expect(shortApp('trailing.')).toBe('trailing')
  })
})

describe('bandParts', () => {
  test('draws nothing without a server', () => {
    expect(bandParts(null, 100)).toEqual([])
  })
  test('not connected', () => {
    expect(text({ connection: null, verdict: null })).toBe('▍wendy  not connected')
    expect(bandParts({ connection: null, verdict: null }, 100)[2]).toEqual({ text: 'not connected', dim: true })
  })
  test('connected, no verdict', () => {
    expect(text(CONNECTED)).toBe('▍wendy  hopeful-glider.local · lan')
    const parts = bandParts(CONNECTED, 100)
    expect(parts[0]).toEqual({ text: '▍wendy', color: COLORS.label, bold: true })
    expect(parts[2]).toEqual({ text: 'hopeful-glider.local', color: COLORS.device, bold: true })
    expect(parts[3]).toEqual({ text: ' · lan', dim: true })
  })
  test('each verdict kind has its mark and color', () => {
    const cases: [Omit<BandVerdict, 'app' | 'target'>, string, string][] = [
      [{ kind: 'healthy', label: 'healthy' }, '✓ demo healthy', COLORS.healthy],
      [{ kind: 'readiness-unknown', label: 'readiness unknown' }, '? demo readiness unknown', COLORS.caution],
      [{ kind: 'failing', label: 'crash-looping' }, '✗ demo crash-looping', COLORS.failing],
      [{ kind: 'not-verified', label: 'not verified' }, '– demo not verified', COLORS.caution],
    ]
    for (const [verdict, shown, color] of cases) {
      const parts = bandParts(withVerdict(verdict), 100)
      expect(parts[parts.length - 1]).toEqual({ text: shown, color })
      expect(text(withVerdict(verdict))).toBe(`▍wendy  hopeful-glider.local · lan   ${shown}`)
    }
  })
  test('a verdict from another device is not shown', () => {
    const other = withVerdict({ kind: 'healthy', label: 'healthy' }, { device: 'other-pi.local:50051', transport: 'lan' })
    expect(text(other)).toBe('▍wendy  hopeful-glider.local · lan')
  })
  test('the colors are the CLI palette', () => {
    expect(COLORS).toEqual({ label: '#34d399', device: '#6ee7b7', healthy: '#10b981', caution: '#f59e0b', failing: '#ef4444' })
  })
  test('below 60 columns the transport is dropped and gaps narrow', () => {
    expect(text(withVerdict({ kind: 'healthy', label: 'healthy' }), 59)).toBe('▍wendy  hopeful-glider.local ✓ demo healthy')
    expect(text(CONNECTED, 60)).toBe('▍wendy  hopeful-glider.local · lan')
  })
  test('an empty transport is left out', () => {
    const band: Band = { connection: { device: 'pi', transport: '', target: { device: 'pi', transport: '' } }, verdict: null }
    expect(text(band)).toBe('▍wendy  pi')
  })
})
