import type { McpToolResult } from 'claude-code'

/** An MCP success result: JSON in structuredContent and as text (go/internal/cli/mcp/results.go:13). */
export function mcpJson(value: object): McpToolResult {
  return { content: [{ type: 'text', text: JSON.stringify(value) }], isError: false, structuredContent: value }
}

/** The same result when the client drops structuredContent. */
export function mcpTextOnly(value: object): McpToolResult {
  return { content: [{ type: 'text', text: JSON.stringify(value) }], isError: false }
}

/** An MCP error result (go/internal/cli/mcp/errors.go:29). */
export function mcpError(code: string, message: string): McpToolResult {
  return { content: [{ type: 'text', text: `[${code}] ${message}` }], isError: true, structuredContent: { error_code: code, message } }
}

// wendy_status, connected: go/internal/cli/mcp/tools_status.go:44-60
export const STATUS_CONNECTED = {
  connected: true,
  tool_groups: ['core'],
  cli_version: '2026.10.01-013419',
  cli_update: { available: false },
  installation_planning: true,
  installation_jobs: true,
  simulator_management: true,
  device: 'hopeful-glider.local',
  connection_type: 'lan',
  suggested_next_step: 'Connected to hopeful-glider.local via lan. Use run or inspect containers and logs; enable specialist groups with wendy_tools.',
  proxy_diagnostics: [],
  command_target: { device: 'hopeful-glider.local:50051', transport: 'lan' },
}

// wendy_status, connected to a different device than RUN_STARTED.target
export const STATUS_OTHER_DEVICE = {
  ...STATUS_CONNECTED,
  device: 'other-pi.local',
  suggested_next_step: 'Connected to other-pi.local via lan. Use run or inspect containers and logs; enable specialist groups with wendy_tools.',
  command_target: { device: 'other-pi.local:50051', transport: 'lan' },
}

// wendy_status, not connected: go/internal/cli/mcp/tools_status.go:24-36
export const STATUS_DISCONNECTED = {
  connected: false,
  suggested_next_step: 'Call device_list, then device_connect with the returned device selector. For uninstalled hardware enable setup with wendy_tools, then use os_install_plan. An empty scan does not establish a network failure.',
  tool_groups: ['core'],
  cli_version: '2026.10.01-013419',
  cli_update: { available: false },
  installation_planning: true,
  installation_jobs: true,
  simulator_management: true,
  proxy_diagnostics: [],
}

// run, started: go/internal/cli/mcp/tools_run.go:138-171
export const RUN_STARTED = {
  target: { device: 'hopeful-glider.local:50051', transport: 'lan' },
  output: 'Deployed sh.wendy.demo',
  truncated: false,
  readiness: 'not_checked',
  status: 'started',
  suggested_next_step: "Connect to the returned target, check container_list and telemetry_logs, then test the app's health endpoint or ROS interface. Deployment alone does not verify behavior.",
}

// run with start: false: tools_run.go:163-165
export const RUN_CREATED = { ...RUN_STARTED, status: 'created' }

// run, failed (the result is also marked isError): tools_run.go:142-160
export const RUN_FAILED = {
  target: { device: 'hopeful-glider.local:50051', transport: 'lan' },
  output: 'error: docker build failed',
  truncated: false,
  readiness: 'not_checked',
  error_code: 'INTERNAL',
  status: 'failed',
  message: 'Deployment did not complete. Inspect the output and device state before retrying; a timed-out or cancelled command may already have created or started the container.',
}

// run from a CLI older than 2026.09.30: no target, no readiness
export const RUN_NO_TARGET = { output: 'Deployed sh.wendy.demo', truncated: false, status: 'started' }

export const WENDY_JSON = '{\n  "appId": "sh.wendy.demo",\n  "version": "0.1.0",\n  "readiness": { "tcpSocket": { "port": 8080 } }\n}\n'

// app_inspect: go/internal/cli/mcp/tools_app_inspect.go:110-141 (state), 197-262 (recent_logs), 379-453 (readiness)
const INSPECT_BASE = {
  app_name: 'sh.wendy.demo',
  usage: { 'sh.wendy.demo': { container_name: 'sh.wendy.demo', status: 'reported' } },
  recent_logs: { status: 'observed', records: [], min_severity: 13, collection_limited: false },
}

const READY_PASSED = {
  status: 'passed',
  checks: [{ kind: 'tcp_socket', port: 8080, status: 'passed' }],
  configuration_source: 'local_project',
  deployed_configuration_verified: false,
  scope: 'declared TCP connectivity only',
}

const READY_NO_PROBES = {
  status: 'unknown',
  checks: [],
  configuration_source: 'local_project',
  deployed_configuration_verified: false,
  reason: 'project declares no TCP readiness probes',
}

const RUNNING = { running_state: 'RUNNING', version: '0.1.0', failure_count: 0, last_exit: { status: 'unknown' }, services: [], all_services_running: true }

export const INSPECT_HEALTHY = { ...INSPECT_BASE, state: RUNNING, readiness: READY_PASSED }

export const INSPECT_NO_PROBES = { ...INSPECT_BASE, state: RUNNING, readiness: READY_NO_PROBES }

export const INSPECT_CRASH_LOOPING = {
  ...INSPECT_BASE,
  state: { running_state: 'CRASH_LOOPING', version: '0.1.0', failure_count: 3, last_exit: { status: 'recorded', reason: 'OOMKilled', code: 137 }, services: [], all_services_running: false },
  recent_logs: {
    status: 'observed',
    min_severity: 13,
    collection_limited: false,
    records: [
      { body: 'loading model weights', severityText: 'WARN', severityNumber: 13, is_history: true },
      { body: { message: 'allocation failed', bytes: 2147483648 }, severityText: 'ERROR', severityNumber: 17, is_history: true },
      { body: 'Traceback (most recent call last):\n  File "app.py", line 9\nMemoryError', severityText: 'ERROR', severityNumber: 17, is_history: true },
      { body: 'Killed', severityText: 'ERROR', severityNumber: 17, is_history: true },
    ],
  },
  readiness: {
    status: 'failed',
    checks: [{ kind: 'tcp_socket', port: 8080, status: 'failed', reason: 'app readiness requires every service to be running' }],
    configuration_source: 'local_project',
    deployed_configuration_verified: false,
    scope: 'declared TCP connectivity only',
  },
}

export const INSPECT_STOPPED_EXIT = {
  ...INSPECT_BASE,
  state: { running_state: 'STOPPED', version: '0.1.0', failure_count: 1, last_exit: { status: 'recorded', reason: 'Error', code: 1 }, services: [], all_services_running: false },
  readiness: READY_NO_PROBES,
}

export const INSPECT_STOPPED_UNKNOWN = {
  ...INSPECT_BASE,
  state: { running_state: 'STOPPED', version: '0.1.0', failure_count: 0, last_exit: { status: 'unknown' }, services: [], all_services_running: false },
  readiness: READY_NO_PROBES,
}

export const INSPECT_SERVICE_DOWN = {
  ...INSPECT_BASE,
  state: {
    running_state: 'RUNNING',
    version: '0.1.0',
    failure_count: 0,
    last_exit: { status: 'unknown' },
    services: [
      { name: 'api', container_name: 'sh.wendy.demo_api', running_state: 'RUNNING' },
      { name: 'worker', container_name: 'sh.wendy.demo_worker', running_state: 'STOPPED' },
    ],
    all_services_running: false,
  },
  readiness: READY_NO_PROBES,
}

export const INSPECT_READINESS_FAILED = {
  ...INSPECT_BASE,
  state: RUNNING,
  readiness: {
    status: 'failed',
    checks: [{ kind: 'tcp_socket', port: 8080, status: 'failed', reason: 'TCP connection could not be established: dial tcp 10.0.0.5:8080: connect: connection refused' }],
    configuration_source: 'local_project',
    deployed_configuration_verified: false,
    scope: 'declared TCP connectivity only',
  },
}

// Crashed and was restarted between two checks: running again, failure_count up.
export const INSPECT_RECOVERED = { ...INSPECT_BASE, state: { ...RUNNING, failure_count: 2, last_exit: { status: 'recorded', reason: 'Error', code: 1 } }, readiness: READY_NO_PROBES }

// No state at all (unexpected shape).
export const INSPECT_MALFORMED = { app_name: 'sh.wendy.demo', readiness: READY_PASSED }

// container_list: go/internal/cli/mcp/tools_container.go:116-145
export const CONTAINERS_RUNNING = {
  containers: [{ app_name: 'sh.wendy.demo', app_version: '0.1.0', failure_count: 0, running_state: 'RUNNING' }],
}

export const CONTAINERS_CRASHED = {
  containers: [{ app_name: 'sh.wendy.demo', app_version: '0.1.0', exit_code: 1, failure_count: 0, running_state: 'STOPPED', termination_reason: 'crashed' }],
}
