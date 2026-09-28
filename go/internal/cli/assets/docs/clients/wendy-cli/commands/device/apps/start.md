Starts an app by name. If the app-name argument is not provided, and the terminal is interactive - a list of all uploaded apps is shown. You can then interactively start an app.

By default, after starting the app the CLI attaches to the container's output stream and prints logs to the terminal. Press **Ctrl-C** to detach.

An attached start (the default) keeps the restart policy the app was deployed with. A detached start (`--detach`) sets the app's restart policy to **`unless-stopped`** and saves it on the device, replacing the deployed policy: the agent restarts the app whenever it exits, and after a reboot, until it is explicitly stopped with `wendy device apps stop`.

If the container keeps exiting and the agent has already performed at least one automatic restart, `wendy device apps list` shows the app as **crash-looping** (a red `↻` icon) rather than stopped, so a restart loop is not mistaken for a clean exit. Use `wendy device logs --app <name>` to view the crash output.

## Flags

| Flag | Description |
|------|-------------|
| `-d`, `--detach` | Start the app and return once the agent confirms it has started, without streaming output. Also sets the app's restart policy to `unless-stopped` (see above). |

## Examples

Start an app and stream its output:

```sh
wendy device apps start my-app
```

Start an app in the background (detached):

```sh
wendy device apps start --detach my-app
wendy device apps start -d my-app
```

When `--detach` is used, the CLI waits for the agent to confirm the container has started, then returns without streaming any output. If the agent reports an error or closes the stream before confirming the start, the command exits with a non-zero status instead of reporting success.

## Reported outcome

When an attached start returns, the CLI reports the app's state at that point: `started` if it is still running (for example a multi-service app), `stopped` — or a short reason such as `crashed (exit 1)` — if it has exited, or `crash-looping` if it keeps restarting.

## JSON output

With `--json` (automatic when stdout is not a terminal) the command prints one JSON object to stdout. An attached start sends the app's own output to stderr, so stdout holds only the result.

```json
{"app": "my-app", "action": "start", "status": "started", "restartPolicy": "unless-stopped"}
```

`status` is `started` for a detached start. For an attached start it is the app's state when its output stream ended (`running`, `stopped` or `crash_looping`), with `exitCode` and `terminationReason` when the agent recorded why the app last exited.
