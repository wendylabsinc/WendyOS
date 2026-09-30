Removes an app from the device. With an `[app-name]` argument, removes that app;
otherwise lists all apps and allows interactive removal.

```sh
wendy device apps remove [app-name] [flags]
```

## Flags

| Flag | Description |
|------|-------------|
| `--cleanup` | Also remove the container image, reclaiming disk space. |
| `--delete-volumes` | Also delete the app's persistent volumes. |
| `--force` | Skip confirmation prompts. Required without an interactive terminal. |

When neither `--cleanup` nor `--delete-volumes` is given and `--force` is
absent, the command shows an interactive "Also clean up?" checklist offering to
delete the container image and the persistent volumes, so the cleanup choices
are still reachable without passing flags.

Without an interactive terminal there is nobody to answer the confirmation, so `remove` refuses to run unless `--force` is given, exiting with status 2 (a usage error).

## JSON output

With `--json` (automatic when stdout is not a terminal) the command prints one JSON object to stdout. `deleteImage` and `deleteVolumes` appear when those clean-ups were requested; `status` is `cancelled` if the removal was declined at the prompt.

```json
{"app": "my-app", "action": "remove", "status": "removed", "deleteImage": true}
```
