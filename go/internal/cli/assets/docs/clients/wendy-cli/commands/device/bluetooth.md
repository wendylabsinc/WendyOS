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

### Mesh radio mode

Inspect the available mesh Bluetooth modes, or select one:

```sh
wendy device bt mode
wendy device bt mode coded-s8
```

Wendy Lite on supported ESP32-C5/C6 firmware offers `normal`, `coded-s2`, and
`coded-s8`. A mode covers advertising, scanning and connected data, rather than
changing only an established connection. Changing the Lite profile restarts the
device; the CLI reports that restart. It preserves whether Wendy Net is enabled
and the other saved carrier choices. Use `wendy device wendy-net configure` to
turn carriers on or off.

Coded modes require support at both ends. S2 and S8 peers can discover one
another, but an ESP in normal mode does not scan coded advertisements. Coded
modes reduce throughput and increase airtime; walls and interference still limit
range. WendyOS reports the selected adapter's capabilities and rejects a coded
mode when its controller or BlueZ cannot select the complete advertising and
data profile. A generic Coded capability alone does not promise S2/S8 selection.

### Transmission power

Inspect capabilities and power settings, or change either setting independently:

```sh
wendy device bt power
wendy device bt power --advertising maximum --connections maximum
wendy device bt power --advertising -6
wendy device bt power --advertising default --connections default
```

Power accepts `default`, `maximum`, or a value in dBm. Omitted settings are
preserved and explicit changes survive restart. The policy covers Wendy-owned
Bluetooth advertisements and, where supported, connected links. It also applies
when Wendy Net is disabled. It does not change Wi-Fi power or another program's
independently managed advertising instances.

ESP32-C5/C6 support independent advertising and connection settings. Their
controller uses discrete levels from -15 to +20 dBm; a numeric request between
levels rounds down. Connection settings also cover scanning/initiation and are
applied to existing and future links. `default` restores the firmware's controller
default, while `maximum` requests the highest supported level.

WendyOS checks the selected adapter's advertised power range. Advertising power
can be set only when BlueZ exposes that capability. A connected-link request
other than `default` is rejected when the backend cannot set it. Requested power,
registration success and actual controller readback are separate fields: an
absent selected value means it has not been measured. Lite may briefly report
`pending` while the change is applied after the command reply is delivered.
