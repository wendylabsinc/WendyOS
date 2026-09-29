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

## Constraints

Reuse existing Go CLI, VM, onboarding, and gRPC services. Host backends are injected
where importing commands would create a dependency cycle. Preserve direct/cloud
target identity. No credentials in results. No interactive prompts on MCP stdio.
Unitree G1 PC2 retains vendor Ubuntu. New host workflow tests use fixtures and fake
backends; development tests must not flash hardware or install host dependencies.
