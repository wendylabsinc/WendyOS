# `wendy device bluetooth`

Manages Bluetooth peripherals on WendyOS and radio settings on supported WendyOS and Wendy Lite devices. The `bt` alias is also accepted; examples use the full `bluetooth` command name.

On WendyOS, run without a subcommand to browse and manage peripherals in an interactive table. Use `mode` and `power` for radio settings on WendyOS or supported Lite firmware. These controls affect the selected device, not the computer running the CLI. Add `--device <device>` to select a device explicitly.

For ordinary Wendy Net connections, start with `normal` mode and controller-default power. Choose a coded profile only when both peers report support and you need to trade throughput for radio range. Changing transmit power and choosing a coded profile are separate decisions.

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


### `wendy device bluetooth mode`

Read the current profile and status before making a change:

```sh
wendy device bluetooth mode --device <device>
```

| Target | Profiles available in this implementation |
| --- | --- |
| Wendy Lite ESP32-C5/C6 with radio-profile firmware | `normal`, `coded-s2`, `coded-s8` |
| WendyOS using Linux/BlueZ | `normal`; `coded-s2` and `coded-s8` when advertising and management PHY capability checks succeed. S2/S8 are connected-data coding preferences; advertising coding is selected by the controller. |

Older Lite firmware may support only `normal`; a coded request is rejected unless
the firmware explicitly reports support. The Lite controls require firmware with
Bluetooth radio management support.

| Profile | When to use it |
| --- | --- |
| `normal` | Start here for ordinary connections and compatibility with older adapters. Uses 1M advertising; Lite uses 1M data. |
| `coded-s2` | Use between supported peers to prefer S2 coding for connected data (500 kbit/s). |
| `coded-s8` | Use between supported peers to prefer stronger S8 coding for connected data (125 kbit/s), trading throughput and airtime for range. |

These are radio rates, not application throughput or a promised distance. Walls,
interference and shared Wi-Fi airtime still affect results. A Lite profile covers
mesh advertising, scanning and connected data. On WendyOS, a coded profile enables
Coded mesh advertising, scanning and connection initiation, then requests the
selected S2/S8 preference on Wendy-owned mesh links. BlueZ retains ownership of
advertising and discovery. Management advertisements on WendyOS and guest BLE
advertisements on Lite remain on 1M.

WendyOS scanning and connection-initiation PHY selection applies to the selected
adapter, so it can also affect other applications using that adapter. Connected
PHY requests are limited to Wendy-owned mesh links.

WendyOS does not promise a specific advertising coding scheme or guarantee the
peer/controller will choose the requested connected-data coding. The tested
Intel controller supports Coded PHY but not the advertising-coding-selection HCI
command. Read `meshModeDetail` for these limits. Status can confirm Coded PHY,
but HCI PHY readback does not distinguish S2 from S8. Firmware with advertising
coding selection, such as the supported Lite profiles, can select that scheme
for advertisements as well.

To change the profile of an already configured device:

```sh
wendy device bluetooth mode coded-s2 --device <device>
```

This preserves whether Wendy Net participates and which carriers are enabled.
It does not enable a disabled BLE carrier. On WendyOS, an explicit profile is
persisted and applied to scanning/initiation, the mesh advertisement and current
Wendy-owned mesh links; failed application is reported. A changed Lite profile can restart the
device; the CLI reports the restart. Reconnect after it boots and inspect the
profile again. On Lite, repeating an unchanged setting does not request a new
profile change.

To enable BLE and select its profile together, use the Wendy Net command:

```sh
wendy device wendy-net configure --device <device> \
  --participate=true --ble-mode=normal
wendy device wendy-net status --device <device>
```

`--ble-mode` accepts `off`, `normal`, `coded-s2`, or `coded-s8`. A value other than
`off` enables the BLE carrier and selects the profile; `off` disables that carrier
while retaining the saved profile. It does not change participation unless you
also supply `--participate`. Choose `--ble-mode` or `--ble`, never both.
`bluetooth mode` itself does not accept `off`.

A C6 permits one mesh carrier at a time. If switching it from mesh Wi-Fi to BLE,
include `--infrastructure-wifi=false` in the configure command. To disable its
only carrier, also stop participation or select another supported carrier.
See [Lite peer setup](/docs/advanced/development/local-mesh-radio).

Configure coded peers through USB or another working management path. A normal
ESP scanner cannot discover coded advertisements. WendyOS needs a selected
adapter that passes the Coded capability checks. S2 and S8 peers can discover each other,
but use the same profile at both ends when comparing range. Allow the reported
restart to finish, reconnect, and check peer connectivity on each device. A saved
profile alone does not prove a working connection.

To return a Lite device to normal mode through your working management path:

```sh
wendy device bluetooth mode normal --device <device>
```

On Lite, `mode` prints saved network settings and runtime state; `--json` also
includes the firmware's `bleProfilesSupported` bitmask (bits 0, 1 and 2 mean
normal, S2 and S8). Its text view shows the saved profile, not a list of supported
profiles. On WendyOS, `mode` always prints JSON. Check `supportedMeshModes` and
`codedUnavailableReason` for this adapter's support, and `links` for observed
PHYs and connected-link power. Each link's `txPhy`/`rxPhy` uses 1 for 1M, 2 for
2M and 3 for Coded; this does not identify the coding scheme. Optional
`currentDbm`/`maximumDbm` values are controller readbacks, not adjustable settings;
missing values are unknown. Unsupported and reserved power responses are not
presented as measurements.

### `wendy device bluetooth power`

Inspect support and current policy first:

```sh
wendy device bluetooth power --device <device>
```

Advertising power affects transmissions used to announce the device. Connection
power affects traffic on established links; on Lite it also covers scan requests
and connection initiation. Raising advertising power alone does not raise
connected-link power. These settings are independent of the mesh profile and
apply even while Wendy Net is disabled. They cover Wendy-owned Bluetooth
advertisements and supported links, including management and guest Bluetooth on
Lite. They do not change Wi-Fi power or another program's independent advertising
instances.

Devices without a saved power policy use the firmware/controller default.
`default` returns a setting to that default; `maximum` is an explicit opt-in.
Existing saved choices are retained after a firmware update. Higher transmit
power does not change the other peer's power or guarantee a usable two-way link.

| Target | Advertising power | Connection power |
| --- | --- | --- |
| Supported Lite ESP32-C5/C6 firmware | `default`, `maximum`, or an integer from -15 to +20 dBm | Same choices; applies to existing and future links |
| WendyOS Linux/BlueZ | `default`; `maximum` or integer dBm only when the selected adapter exposes setting support and a valid range | `default` only; an explicit connected-power setter is not implemented. Current/maximum power readbacks are available when supported by the controller |

For a WendyOS adapter that reports advertising-power support:

```sh
wendy device bluetooth power --device <device> --advertising maximum
```

For supported Lite firmware, explicitly request maximum power for both settings:

```sh
wendy device bluetooth power --device <device> \
  --advertising maximum --connections maximum
```

To reduce only advertising power, preserving the connection setting:

```sh
wendy device bluetooth power --device <device> --advertising -6
```

Use whole numbers without a `dBm` suffix. Lite uses discrete levels from -15 to
+20 dBm and rounds an in-between request down; for example, -5 requests -6 dBm.
`maximum` requests +20 dBm on C5/C6. WendyOS validates against the selected
adapter's reported range. Choose values within the target's supported range.

To restore both settings to the firmware/controller default:

```sh
wendy device bluetooth power --device <device> \
  --advertising default --connections default
```

Omitted settings are preserved and explicit changes survive restart. Power
changes do not require a device reboot. On Lite, changes apply after the command
reply has drained; use USB or ordinary Wi-Fi management when reducing power could
make your Bluetooth management link unreachable.

`power` always prints JSON. Read the status again after a change:

```sh
wendy device bluetooth power --device <device>
```

| Output | What to check |
| --- | --- |
| Lite `desired`, `applied`, `pending`, `error` | Saved intent is separate from application. If `pending` is true, query again; check `error` if application fails. |
| Lite `advertisingKnown`, `connectionDefaultKnown`, and each connection's `currentKnown` | Treat corresponding dBm values as measured only when their known flag is true. `supportedLevelsDbm` lists supported levels. |
| WendyOS `advertisingSetSupported`, `connectionsSetSupported` | Check these before requesting non-default power. This backend reports connection setting support as false. |
| WendyOS `advertising`, `advertisements`, `error` | `advertising` is saved policy; per-advertisement `requestedDbm` and `registered` describe the request and registration. Missing `selectedDbm` means actual power was not measured. |

On WendyOS a policy can be saved even if an active advertisement fails to apply
it; the command reports that error. Inspect status and correct the policy or
restore `default`. Registration success alone does not prove measured transmit
power or improved range.

## Flags

| Flag | Description |
| --- | --- |
| `--device <device>` | Select the target device explicitly. |
| `--pair` | Pair with the peripheral when connecting (default `true`, `connect` only). |
| `--trust` | Trust the peripheral when connecting (default `true`, `connect` only). |
| `--advertising <setting>` | Change advertising power (`power` only); omitted means preserve. |
| `--connections <setting>` | Change connection power (`power` only); omitted means preserve. |
| `--json` | Request JSON for `list` and Lite `mode`. `power` and WendyOS `mode` already print JSON. |
