# Cloud Discover

`wendy cloud discover` lists devices enrolled in Wendy Cloud.

## Usage

```sh
wendy cloud discover [flags]
```

## Output

In an interactive terminal the command shows a **live TUI** that auto-refreshes every 10 seconds. Each row displays the device name and type from the Cloud roster. Browsing and refreshing the list do not open device tunnels or query devices for hardware or agent versions.

The version column shows `—` unless a version was learned through an explicit update check. Wendy Lite firmware versions come directly from the Cloud roster. In non-interactive environments or when `--json` is passed, a JSON array is written to stdout. IP addresses remain available in JSON output.

### TUI keyboard shortcuts

| Key | Action |
|-----|--------|
| `↑` / `↓` | Navigate the device list |
| `enter` | Copy selected device info as JSON to clipboard |
| `a` | Copy all devices as JSON to clipboard |
| `u` | Update the selected device's agent binary in place |
| `q` / `Ctrl+C` | Quit |

If no devices are found, a descriptive message is shown instead:

- Without `--all`: `No online devices found. Use --all to include offline devices.`
- With `--all`: `No enrolled devices found.`

## Asset limit

`cloud discover` caps results at **10,000 devices**. If more are returned, the command exits with an error:

```
cloud returned more than 10000 devices
```

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--all` | `false` | Include offline (not currently connected) devices. By default only online devices are shown. |
| `--broker-url` | `$WENDY_BROKER_URL` (or derived from cloud endpoint) | Tunnel broker `host:port` used for explicit in-place updates. |
| `--cloud-grpc` | `""` | Cloud gRPC endpoint. Overrides session selection. When multiple sessions are stored and no default is set, an interactive terminal shows a session picker; a non-interactive environment errors. |

## Related

- [`wendy cloud tunnel`](./tunnel.md) — open a tunnel to a cloud device
- [Cloud connectivity](./connectivity.md)
- [Cloud requirements](./requirements.md)
