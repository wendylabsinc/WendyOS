# Robot companion

A small app for testing Wendy in ChatGPT. It exposes `get_status` and
`set_message` over MCP on port 8091. It does not move hardware, activate a camera,
or play audio. The message is kept in memory and resets when the app restarts.
The MCP listener binds only to loopback. Host networking lets Wendy Agent reach
that listener through its authenticated StreamMCP service.

```sh
wendy run --yes --detach --build-type docker --device <device> --prefix Examples/RobotCompanion
wendy --json device apps list --device <device>
```

Use the [ChatGPT gateway guide](../../plugins/wendy-chatgpt/README.md) to allow
this app and export its tools. A successful `get_status` response establishes
application readiness separately from the container's running state.
