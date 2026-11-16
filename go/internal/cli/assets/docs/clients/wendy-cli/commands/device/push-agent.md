> **Developer / diagnostic tool.** This command is hidden from `wendy device --help`. It is not part of the normal update workflow.

Uploads a locally-built `wendy-agent` binary to the device using the same `UpdateAgent` RPC that `wendy device update` uses, bypassing the GitHub release fetch. After upload the CLI waits for the restarted agent to become reachable, then verifies what the device is running:

- When the agent reports the SHA-256 of its running executable and it matches the uploaded binary, the command confirms the device is running your binary and prints the version it reports. This is the only proof that matters for a dev-over-dev push, since dev builds share identical version strings.
- When the running hash does **not** match the uploaded binary, the command exits non-zero — the push did not land.
- When the agent reports no hash (the macOS agent, agents from releases predating the field) but the upload stream was confirmed, the command says the push could not be verified and exits 0.
- When the upload stream was **not** confirmed *and* the agent reports no hash, nothing proves the push landed, so the command exits non-zero and asks you to check the device.

An unconfirmed upload is expected rather than exceptional here: the agent restarts the moment the binary lands, which can close the stream before its acknowledgement arrives.

## Usage

```sh
wendy device push-agent <path-to-agent-binary>
```

## When to use this

Use `push-agent` when you need a patched or development agent running on a device to capture verbose engine diagnostics (e.g. raw EFI variables via `wendy os update-status --json`) without SSH access. For routine agent updates, use [`wendy device update`](./update.md).

## Difference from `wendy device update --binary`

`wendy device update --binary` performs an OS-update check after uploading the binary and re-uploads the binary if an OS update is applied. `push-agent` performs no OS update check — it uploads the binary and waits for the agent restart only.

Both commands hash-verify the running binary after the restart (see [`device update` verification](./update.md#verification)), but `push-agent` reconnects by address only rather than through the full reconnect path `device update` uses, so it does not follow a device that comes back at a different address.

## Arguments

| Argument | Description |
|---|---|
| `<path-to-agent-binary>` | Path to the compiled `wendy-agent` binary to upload. |

## Exit codes

| Code | Meaning |
|---|---|
| `0` | Agent restarted and the uploaded binary was verified by hash — or the upload was confirmed and the agent reports no hash. |
| non-zero | Binary could not be read, upload failed, the agent did not come back online, the running binary's hash does not match the upload, or the upload was unconfirmed and the agent reports no hash. |
