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
	"vm:NAME for a local simulator, or a cloud selector such as cloud://HOST:PORT/org/ID/asset/ID. " +
	"`run` reuses the connected target. If you pass device to deploy elsewhere, connect to that device before verifying. " +
	"With no connection and no device, `run` fails with NOT_CONNECTED instead of guessing.\n\n" +
	"Deploy, then verify: pass project_path as the absolute project directory. " +
	"`run` returns status, target and a build-log tail; readiness is not_checked. " +
	"Check `container_list` (running_state, termination_reason), read `telemetry_logs` for startup errors, " +
	"and test the app itself, e.g. its HTTP port. A first build can take minutes: raise timeout_seconds (default 300). " +
	"AUTH_REQUIRED means the user must run `wendy auth login` in a terminal.\n\n" +
	"MCP or CLI: prefer these tools for device state, containers, logs and deploys; " +
	"results are structured and reuse this session's connection. " +
	"Only core tools are listed at first: enable setup, simulator, hardware, robotics, observability or cloud tools with `wendy_tools`. " +
	"Use the CLI for work without a tool, such as `wendy auth login` and `wendy run --watch`. " +
	"In a shell, keep each flag and its value as separate words (wendy run --device \"$DEVICE\"). " +
	"Avoid DEV=\"--device robot\": zsh passes it as one argument. " +
	"Relay CLI update notices to the user with the update command and MCP server restart step.\n\n" +
	"More: read the wendy://guide resource for workflows, entitlements, error codes and links to wendy://docs/."
