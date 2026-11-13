package mcp

// serverInstructions is returned in the MCP initialize result. Clients that
// defer tool schemas (Claude Code) or treat instructions as standing guidance
// (Codex) load only this text and the tool names at session start, so it must
// stand on its own. Keep the first paragraph complete within 512 bytes so a
// client that truncates still learns where to start and how to deploy, keep
// the whole text ASCII and at most 2048 bytes, and name only core tools, which
// are advertised without enabling a group; instructions_test.go enforces all
// of this.
const serverInstructions = "Wendy MCP manages WendyOS edge devices (Raspberry Pi, Jetson, x86 boards) and deploys apps to them. " +
	"Call `wendy_status` first: it reports the connection and a suggested next step. " +
	"Most tools need the one active device connection: find devices with `device_list` (scan=true adds LAN), " +
	"then connect with `device_connect`. " +
	"`run` builds a project and deploys it to the connected device; then verify with `container_list` and `telemetry_logs`, " +
	"because a started container is not a working app.\n\n" +
	"Targets: `device_connect` and `run` take a device selector from `device_list` unchanged: host:port, " +
	"cloud://HOST:PORT/org/ID/asset/ID, or vm:NAME for a local simulator. " +
	"No hardware? Connect a listed simulator rather than skip on-device checks; ask the user before creating one. " +
	"`run` reuses the connected target. To deploy elsewhere, pass device and connect to it before verifying. " +
	"With no connection and no device, `run` fails with NOT_CONNECTED instead of guessing.\n\n" +
	"Deploy, then verify: pass project_path as the absolute project directory. " +
	"`run` returns status, target and a build-log tail; readiness is not_checked. " +
	"Check `container_list` (running_state, termination_reason), read `telemetry_logs` for startup errors, " +
	"and test the app itself, e.g. its HTTP port. A first build can take minutes: raise timeout_seconds (default 300). " +
	"On AUTH_REQUIRED call `auth_login` and show the user its link (else: `wendy auth login` in a terminal).\n\n" +
	"MCP or CLI: prefer these tools for device state, containers, logs and deploys. " +
	"Only core tools are listed at first: enable setup, simulator, hardware, robotics, observability or cloud tools with `wendy_tools`. " +
	"Use the CLI where no tool exists, such as `wendy run --watch`. " +
	"In a shell, keep each flag and its value as separate words (wendy run --device \"$DEVICE\"). " +
	"Avoid DEV=\"--device robot\": zsh passes it as one argument. " +
	"Relay CLI update notices to the user with the update command and MCP server restart step.\n\n" +
	"More: read wendy://guide for workflows, entitlements, error codes and links to wendy://docs/."

// relayInstructions replace serverInstructions on the relay channel (`wendy
// cloud mcp`): a hosted chat has no shell, no tool groups and no CLI to
// update, so it gets only what the relay can do and how calls are confirmed.
const relayInstructions = "Wendy MCP, served through the hosted relay, reaches the WendyOS edge devices of the user who started " +
	"'wendy cloud mcp' in a terminal. Every call runs on that user's machine under their Wendy Cloud session and organization. " +
	"Call `wendy_status` first: it reports the connection and a suggested next step. " +
	"Connect with `device_connect` and a cloud selector (cloud://HOST:PORT/org/ID/asset/ID), " +
	"then inspect the device with `device_info`, `container_list` and `telemetry_logs`.\n\n" +
	"Every permitted tool call waits for confirmation in that terminal by default. --allow-read skips confirmation only for built-in read-only device and Cloud tools that do not access the host. " +
	"--allow-all permits every supported tool without per-call confirmation. Pairing is separate: --auto-accept automatically accepts ChatGPT's verified OAuth client; other clients still require approval. " +
	"A refused call returns DENIED: tell the user what you asked for instead of retrying. " +
	"Tools the user has not allowed are not listed; the user can allow more by restarting 'wendy cloud mcp' with --allow-tool or --allow-all."
