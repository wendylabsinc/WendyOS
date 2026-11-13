/** A deploy target as the Wendy MCP server reports it (`commandTarget`, go/internal/cli/mcp/server.go:31). */
export type WendyTarget = {
  device: string
  transport: string
  cloud_grpc?: string
  selector?: string
}

export type VerdictKind = 'healthy' | 'readiness-unknown' | 'failing' | 'not-verified'

export type BandVerdict = {
  app: string
  target: WendyTarget
  kind: VerdictKind
  label: string
}

export type BandConnection = {
  device: string
  transport: string
  target: WendyTarget
}

/** null: no Wendy server found, so the band draws nothing. */
export type Band = {
  connection: BandConnection | null
  verdict: BandVerdict | null
} | null

declare module 'claude-code' {
  interface PluginState {
    'wendy-mods': { band: Band }
  }
}
