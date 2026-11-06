# `wendy device wifi`

Manages WiFi on the connected WendyOS or Wendy Lite device.
Run with no subcommand to open the WiFi management table on WendyOS, or start
the interactive connection flow on Wendy Lite.

Wendy Lite devices reached over BLE, and devices behind providers that manage
WiFi themselves, use the networks visible from the host machine because they
cannot scan from the device. The [Platform behavior](#platform-behavior)
section below describes that host-side scan.

## Subcommands

### `wendy device wifi list`

Lists WiFi networks visible to the device.

```sh
wendy device wifi list
```

### `wendy device wifi connect`

Interactively selects and connects the device to a WiFi network.

```sh
wendy device wifi connect [--ssid <name>]
```

Pass `--ssid` to skip the interactive network picker.

### `wendy device wifi status`

Shows the device's current WiFi connection.

### `wendy device wifi disconnect`

Disconnects the device from its current network.

### `wendy device wifi rank`

Shows or adjusts the device's network priority order.

### `wendy device wifi forget`

Removes a saved network from the device.

### `wendy device wifi regulatory`

Shows the country and available channels on a Linux WendyOS device. See
[check Wi-Fi country and available channels](#check-wi-fi-country-and-available-channels).

### `wendy device wifi regulatory set-country`

Requests a temporary country hint for the device's physical location. Query the
regulatory state again to verify the result.

---

## Platform behavior

> These notes describe the **host-side scan fallback** described above — how
> the CLI enumerates networks on the machine you are running it from. They do
> not describe scanning on a WendyOS device, which the agent performs.

### macOS

Uses CoreWLAN (`scanForNetworks`) to perform a synchronous, on-demand scan. Results are always current.

Keychain lookup is supported: previously saved "AirPort network password" entries are read automatically, so the password prompt is often skipped.

### Linux

Uses `nmcli device wifi rescan` followed by `nmcli device wifi list`. The rescan is triggered before listing, so results are always current.

Keychain lookup is not supported. The password prompt is always shown when connecting to a network that requires a password.

### Windows

Uses `netsh wlan show networks mode=bssid` to enumerate nearby networks. SSIDs are deduplicated; when a network is visible through multiple BSSIDs (roaming), the strongest signal observed across all BSSIDs is reported. Hidden networks (empty SSID) are skipped.

**Important:** `netsh` reads the Windows WLAN service's cached scan list. The service asks the driver to rescan periodically and may not scan at all while the host is already associated to a network. Results may therefore be stale. If the expected network is missing, wait a few seconds and run the command again, or pass `--ssid` to specify the network directly.

Keychain lookup is not supported. Windows has no equivalent of macOS's auto-stored "AirPort network password" Keychain entry. The password prompt is always shown when connecting to a network that requires a password.

---

## Flags

| Flag | Description |
|------|-------------|
| `--ssid <name>` | Skip the interactive network picker and connect to the named network directly (`connect` only). |

## Wendy Lite devices

Running `wendy device wifi` against a Wendy Lite device starts the same
interactive connection flow as `wendy device wifi connect`: select a network
visible from the host machine, enter its password, and connect the device.

You can also use the explicit subcommands:

```sh
wendy device wifi connect --ssid <network>
wendy device wifi disconnect
```

> **Note:** `wendy device wifi connect` and `wendy device wifi disconnect` stop the running app on the device before applying the new Wi-Fi configuration. If no app is currently running, this step is skipped silently — no warning is printed.


## Check Wi-Fi country and available channels

On a Linux WendyOS device, inspect the current country and channel restrictions.
This requires a device running the agent; local and Docker providers do not
support the command.

Inspect the current state:

```sh
wendy device wifi regulatory --device <device>
```

To request the country where the device is physically located, use its two-letter
country code. For example, for a device in the United Kingdom:

```sh
wendy device wifi regulatory set-country GB --device <device>
wendy device wifi regulatory --device <device>
```

This submits a temporary country hint; it does not override the radio's firmware
or guarantee more channels become available. Changes can take time, so query
again to check the result. The hint does not survive a reboot and does not restart
Wi-Fi, Wi-Fi Aware, or the agent. Devices need `iw` installed; older or non-Linux
agents may report that this operation is unsupported.

### Detailed output and automation

Add `--json` for the full channel report. It includes the kernel's country and
per-radio restrictions, maximum transmit power, permitted widths, and radar-related
restrictions where available. Use `globalCountry` for the kernel hint and
`phys[].country` and `phys[].selfManaged` to distinguish each radio's own state.
A self-managed radio may keep a different country from the kernel hint. `no IR` means the radio cannot initiate transmission
on that channel. Raw output is retained as `rawRegulatory`, `rawPhy` and
`rawChannels` for diagnosis.
