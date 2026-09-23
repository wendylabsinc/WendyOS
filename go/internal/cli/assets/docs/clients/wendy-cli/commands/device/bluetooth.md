# `wendy device bluetooth`

Manages Bluetooth peripherals on the connected WendyOS device. Aliased as `wendy device bt`.

Run without a subcommand to open an interactive table for browsing and managing peripherals; the subcommands below remain available for scripting.

## Interactive TUI

```sh
wendy device bluetooth
```

Results appear quickly — an initial batch of nearby peripherals is delivered after
about 1 s, and a more complete set arrives once the full ~8 s scan window finishes.
Discovered peripherals are deduplicated by address and sorted: connected devices
first, then paired devices, then named devices (alphabetically by name), then
anonymous peripherals (by descending RSSI — strongest signal first). Ties within
each group fall back to address ascending.

Unnamed peripherals are hidden by default. Press `h` to show them or hide them
again. The table shows how many peripherals are hidden. This also applies to the
Bluetooth tab in `wendy device pair`.

| Key | Action |
|-----|--------|
| `↑`/`↓` | Move the selection |
| `←`/`→` | Scroll the table horizontally (when it overflows) |
| `enter` | Connect to the selected peripheral (pairs and trusts it) |
| `d` | Disconnect the selected (connected) peripheral |
| `f` | Forget the selected (paired) peripheral |
| `h` | Show or hide unnamed peripherals |
| `r` | Rescan |
| `q` / `esc` | Quit |

Actions update the table immediately (optimistically) on success. Because a Bluetooth rescan takes several seconds, the table is not rescanned automatically after each action — press `r` to refresh against the device.

## Subcommands

### `wendy device bluetooth list`

Scans for peripherals and prints them as a table, or as JSON with `--json`.
The scan runs in two passes over roughly 8 s total (an early ~1 s partial pass,
then a full pass); `list` waits for the stream to finish before printing, so
expect it to take up to ~8 s to return.

```sh
wendy device bluetooth list [--json]
```

For a connected Bluetooth LE peripheral, the **Link timeout** column shows the link's supervision timeout: how long a silent link lasts before it is dropped. The agent holds BLE HID devices such as gamepads at 0.5 s; `(device)` marks a device whose own, longer value is in force. See [Bluetooth](/docs/hardware/bluetooth#game-controllers-and-other-hid-devices). With `--json` the values are `supervision_timeout_ms` and `requested_supervision_timeout_ms`.

### `wendy device bluetooth connect`

Connects to a peripheral by address.

```sh
wendy device bluetooth connect <address> [--pair] [--trust]
```

### `wendy device bluetooth disconnect`

Disconnects a peripheral by address.

```sh
wendy device bluetooth disconnect <address>
```

### `wendy device bluetooth forget`

Removes the pairing for a peripheral by address.

```sh
wendy device bluetooth forget <address>
```

---

## Flags

| Flag | Description |
|------|-------------|
| `--pair` | Pair with the device when connecting (default `true`, `connect` only). |
| `--trust` | Trust the device when connecting (default `true`, `connect` only). |
| `--json` | Output the scan result as JSON (`list` only). |
