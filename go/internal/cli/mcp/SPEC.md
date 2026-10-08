# Wendy MCP host workflows

## Purpose

Let an assistant prepare, deploy, and diagnose Wendy applications through the same
host and device operations as the CLI. MCP clients need typed operations because
they may have no terminal access. Keep default discovery small and results bounded.

## Requested changes

Implement in the user-approved order:

1. Persistent installation jobs: start, status, resume. Return physical instructions
   promptly, probe hardware after acknowledgment, require exact-target erase
   authorization, and never silently repeat an interrupted or completed write.
2. Simulator list, create, stop, delete, using existing VM and profile support.
3. App inspection: container state, resource usage, recent errors, and explicitly
   unknown readiness or exit details where the agent supplies no evidence.
4. Device OS logs and agent update, distinct from full OS updates.
5. List and close cloud tunnels owned by the current MCP server.
6. Validate project configuration before deployment, with actionable findings.

## Interaction

Start with wendy_status and select specialist groups through wendy_tools. Each
tool returns structured results with a compact text fallback. No custom UI is
required. Installation calls return a durable job ID, state, next action, and
instructions; users perform physical steps between calls. Success after flashing
does not imply successful boot or application readiness.

CLI updates appear in `wendy_status.cli_update` and as a text notice appended
to the next tool result once per release per session. Keep notices independent
of terminal notice state and preserve tool payloads and errors. Relay the update
command and MCP restart step to the user. The injected release checker runs off
the startup path and hourly, respects the existing 24-hour cache and development
build exclusion, and cancels when serving stops. Cached availability is not proof
of a successful latest check; the check timestamp records an attempt.

## Constraints

Reuse existing Go CLI, VM, onboarding, and gRPC services. Host backends are injected
where importing commands would create a dependency cycle. Preserve direct/cloud
target identity. No credentials in results. No interactive prompts on MCP stdio.
Unitree G1 PC2 retains vendor Ubuntu. New host workflow tests use fixtures and fake
backends; development tests must not flash hardware or install host dependencies.

## Skills over MCP

The CLI server and ChatGPT gateway advertise `io.modelcontextprotocol/skills`
under `capabilities.extensions`. Clients call `skills/list`, then `skills/get`
with a catalog URI, and `resources/read` for the instructions and supporting
files. The catalog is static and fits on one terminal page. Unknown cursors and
URIs return protocol errors.

OpenAI imports at most five skills. Package the complete embedded end-user group
as one `wendy` skill, with a workflow index and the original skill directories
under `references/skills/`. Preserve frontmatter, relative links, and source
files; compute SHA-256 digests over the exact returned bytes. Engineering and
unrelated embedded skills are excluded. Serve offline without a device, UI, or
local skill installation. Gateway requests retain their existing authentication.

`scripts/sync-agent-skills.py` generates the same bundle for the ChatGPT plugin
and the embedded server files. Plugin onboarding points to the bundled
`wendy-onboarding` instructions. The native coding plugin points to its original
skill. Onboarding supports first-device installation and first-boot verification,
or a local simulator when hardware is absent. The existing device workspace
prefers fullscreen; onboarding also works through text and tools alone.

The pinned MCP library does not dispatch draft skill methods. A shared dispatcher
handles these two methods at the stdio and HTTP transport boundaries; standard
methods, resources, tool cancellation, and task handling stay with the library.
