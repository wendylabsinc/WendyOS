# Wendy MCP Server Design

**Date:** 2026-04-29  
**Status:** Approved

## Overview

Add a `wendy mcp serve` subcommand that exposes the wendy-agent gRPC API as an MCP (Model Context Protocol) server. This allows AI assistants (Claude, Codex, etc.) to access and debug devices and apps hosted on wendy without any extra tooling beyond the existing `wendy` CLI.

## Architecture

### Location

New cobra subcommand `wendy mcp serve` registered alongside existing commands. The cobra wiring lives in `go/internal/cli/commands/mcp.go`. MCP server logic lives in a new isolated package `go/internal/cli/mcp/`.

### Transport

stdio (JSON-RPC over stdin/stdout) — the standard transport for Claude Desktop, Claude Code, Codex, and all MCP-compatible clients.

### Library

`github.com/mark3labs/mcp-go` — the most mature Go MCP library.

### State Model

```go
type mcpServer struct {
    config *config.Config
    conn   *grpcclient.AgentConnection // nil until connected
    mu     sync.RWMutex
}
```

A single active `AgentConnection` is shared by all tools. Tools that require a connection return a structured error (`"no device connected — use device_connect first"`) if `conn` is nil.

### Startup Sequence

1. `wendy mcp serve [--device <name-or-ip>]` reads `~/.wendy/config.json`
2. If `--device` flag is set or `config.DefaultDevice` is non-empty, auto-connect using `connectWithAutoTLS` (same logic as the CLI)
3. Server starts accepting MCP tool calls on stdio — connected or not

### File Layout

```
go/internal/cli/commands/mcp.go           ← cobra command, flags, entry point
go/internal/cli/mcp/
    server.go                              ← mcpServer struct, tool registration loop
    tools_device.go                        ← device_* tools
    tools_container.go                     ← container_* tools
    tools_telemetry.go                     ← telemetry_* tools
    tools_wifi.go                          ← wifi_* tools
    tools_bluetooth.go                     ← bluetooth_* tools
    tools_hardware.go                      ← hardware_* tools
    tools_filesync.go                      ← filesync_* tools
    tools_provisioning.go                  ← provisioning_* tools
    tools_os.go                            ← os_* tools
```

## Tool Inventory (27 tools)

All tools follow the `service_verb` naming convention.

### Device (5 tools)

| Tool | gRPC Call | Description |
|------|-----------|-------------|
| `device_list` | discovery + providers | Discover devices via mDNS/BLE and local providers |
| `device_connect` | `connectWithAutoTLS` | Connect to a device by name or IP:port |
| `device_disconnect` | conn.Close | Close the active connection |
| `device_info` | `GetAgentVersion` | Get agent version and device metadata |
| `device_set_default` | config write | Write a default device to `~/.wendy/config.json` |

### Container/Apps (6 tools)

| Tool | gRPC Call | Description |
|------|-----------|-------------|
| `container_list` | `ListContainers` | List all containers with state, image, uptime |
| `container_start` | `StartContainer` | Start a stopped container by app name |
| `container_stop` | `StopContainer` | Stop a running container |
| `container_delete` | `DeleteContainer` | Delete a container |
| `container_stats` | `ListContainerStats` | Get CPU/memory/network stats |
| `container_attach` | `AttachContainer` | Capture stdout/stderr (bounded) |

### Telemetry (3 tools)

| Tool | gRPC Call | Description |
|------|-----------|-------------|
| `telemetry_logs` | `StreamLogs` | Stream structured logs, filter by app/service/severity (bounded) |
| `telemetry_metrics` | `StreamMetrics` | Stream metrics snapshot (bounded) |
| `telemetry_traces` | `StreamTraces` | Stream traces snapshot (bounded) |

### WiFi (5 tools)

| Tool | gRPC Call | Description |
|------|-----------|-------------|
| `wifi_list` | `ListWiFiNetworks` | Scan available networks |
| `wifi_connect` | `ConnectToWiFi` | Connect to a network by SSID + password |
| `wifi_status` | `GetWiFiStatus` | Current connection status |
| `wifi_disconnect` | `DisconnectWiFi` | Disconnect from WiFi |
| `wifi_known_networks` | `ListKnownWiFiNetworks` | List saved networks |

### Bluetooth (3 tools)

| Tool | gRPC Call | Description |
|------|-----------|-------------|
| `bluetooth_scan` | `ScanBluetoothPeripherals` | Scan for peripherals (bounded) |
| `bluetooth_connect` | `ConnectBluetoothPeripheral` | Connect to a peripheral by ID |
| `bluetooth_disconnect` | `DisconnectBluetoothPeripheral` | Disconnect a peripheral |

### Hardware (1 tool)

| Tool | gRPC Call | Description |
|------|-----------|-------------|
| `hardware_capabilities` | `ListHardwareCapabilities` | List hardware capabilities |

### File Sync (1 tool)

| Tool | gRPC Call | Description |
|------|-----------|-------------|
| `filesync_sync` | `SyncFiles` | Push/pull files between host and device |

### Provisioning (2 tools)

| Tool | gRPC Call | Description |
|------|-----------|-------------|
| `provisioning_status` | `IsProvisioned` | Check whether the device is provisioned |
| `provisioning_start` | `StartProvisioning` | Start the provisioning flow |

### OS (1 tool)

| Tool | gRPC Call | Description |
|------|-----------|-------------|
| `os_update` | `UpdateOS` | Trigger OS update, stream progress (bounded) |

## Data Flow

### Streaming Tools

`container_attach`, `telemetry_logs`, `telemetry_metrics`, `telemetry_traces`, `bluetooth_scan`, `os_update` all use server-streaming or bidi-streaming gRPC calls. These are handled with bounded collection:

- Accept optional `timeout_seconds` param (default: `5`) and `max_lines` param (default: `100`)
- Open the gRPC stream, collect until either limit is reached or stream closes naturally
- Return all collected output as a single MCP text result
- If the stream errors mid-collection, return whatever was collected plus the error message appended

### Tool Result Format

- Structured data (container list, device info, stats) returned as JSON text
- Plain text for log/trace/attach output
- gRPC status errors unwrapped to human-readable strings (not raw proto)

### Authentication

- On `device_connect`, use `connectWithAutoTLS` which reads certs from `config.CertificateInfo`
- If no certs in config, falls back to insecure connection (same as CLI `Connect()`)
- If mTLS fails, error message instructs user to run `wendy auth login`

## Testing

### Unit Tests

Each `tools_*.go` has a companion `tools_*_test.go`. Tests mock the generated `agentpb` client interfaces. Key cases per file:
- Happy path (mock returns valid response)
- gRPC error propagation (e.g. `NotFound`, `PermissionDenied` → readable string)
- Streaming: timeout cutoff, line-limit cutoff, mid-stream error
- Not-connected path (tools called with `conn == nil`)

### Integration Test

One `mcp_integration_test.go` that spins up an in-process MCP server with a mock agent, calls a representative set of tools end-to-end via the MCP JSON-RPC protocol, and asserts on the returned text content.
