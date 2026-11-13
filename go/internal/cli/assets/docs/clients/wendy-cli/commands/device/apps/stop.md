Stops an app by name. If the app-name argument is not provided, and the terminal is interactive - a list of all uploaded apps is shown. You can then interactively stop an app.

## JSON output

With `--json` (automatic when stdout is not a terminal) the command prints one JSON object to stdout:

```json
{"app": "my-app", "action": "stop", "status": "stopped"}
```
