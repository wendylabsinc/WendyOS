import type { BandConnection, WendyTarget } from '../types'
import { obj, str } from './json'
import type { Obj } from './json'

/** The CLI's plaintext agent port (go/internal/cli/mcp/run_target.go:28). */
const DEFAULT_AGENT_PORT = 50051

/** A device as host:port, as `withDefaultAgentPort` spells a bare host. */
function withDefaultPort(device: string): string {
  if (device.startsWith('[')) {
    return device.includes(']:') ? device : `${device}:${DEFAULT_AGENT_PORT}`
  }
  const colons = device.split(':').length - 1
  if (colons === 1) return device
  if (colons > 1) return `[${device}]:${DEFAULT_AGENT_PORT}`
  return `${device}:${DEFAULT_AGENT_PORT}`
}

/**
 * One identity per target, a cautious port of `runTargetKeys`
 * (go/internal/cli/mcp/run_target.go:82). Equal keys always mean the same
 * device; unequal keys may still be one device, which the verifier then
 * reports as not verified rather than guessing.
 */
export function targetKey(target: WendyTarget): string | undefined {
  if (target.selector) return target.selector
  const device = target.device
  if (device === '') return undefined
  if (device.toLowerCase().startsWith('cloud:') || device.startsWith('vm:')) return device
  if (target.transport === 'cloud') return `name:${device}@${target.cloud_grpc ?? ''}`
  return `addr:${withDefaultPort(device).toLowerCase()}`
}

export function sameTarget(a: WendyTarget | undefined, b: WendyTarget | undefined): boolean {
  if (a === undefined || b === undefined) return false
  const key = targetKey(a)
  return key !== undefined && key === targetKey(b)
}

/** A `commandTarget` from MCP JSON, or undefined when it names no device. */
export function asTarget(value: unknown): WendyTarget | undefined {
  const o = obj(value)
  if (o === undefined) return undefined
  const cloudGrpc = str(o.cloud_grpc)
  const selector = str(o.selector)
  const target: WendyTarget = {
    device: str(o.device),
    transport: str(o.transport),
    ...(cloudGrpc === '' ? {} : { cloud_grpc: cloudGrpc }),
    ...(selector === '' ? {} : { selector }),
  }
  return targetKey(target) === undefined ? undefined : target
}

/** The target a `wendy_status` result is connected to (go/internal/cli/mcp/tools_status.go). */
export function connectedTarget(status: Obj): WendyTarget | undefined {
  if (status.connected !== true) return undefined
  return asTarget(status.command_target) ?? asTarget({ device: status.device, transport: status.connection_type })
}

/** What the band shows for a `wendy_status` result; null when not connected. */
export function connectionOf(status: Obj): BandConnection | null {
  const target = connectedTarget(status)
  if (target === undefined) return null
  return {
    device: str(status.device) || target.device,
    transport: str(status.connection_type) || target.transport,
    target,
  }
}
