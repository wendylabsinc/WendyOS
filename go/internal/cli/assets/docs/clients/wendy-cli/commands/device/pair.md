# `wendy device pair`

Connect peripherals and save their settings on the selected Wendy device.

```sh
wendy device pair --device <device>
```

Use Tab or Shift+Tab to switch between **Bluetooth**, **SensorLink**, **IP Cameras**,
**USB Arms**, and **Unitree**. Discovery runs on the selected device, so it lists
peripherals reachable from that device even when your CLI is elsewhere.

| Key | Action |
| --- | --- |
| Arrow keys | Select a peripheral. |
| Enter | Pair it, or edit an IP camera's login. |
| `f` | Forget the selected saved pairing. |
| `r` | Refresh discovery. |
| `q` / Esc | Quit. |

## Bluetooth peripherals

Select a peripheral and press Enter to connect and pair it. Unnamed peripherals
are hidden; press `h` to show them. Press `d` to disconnect a selected peripheral.
This is separate from Wendy Net's automatic Bluetooth links between devices.

## SensorLink sources

Select a sensor source and press Enter to pair it. Sources must be reachable
from the selected Wendy device, enrolled in the same organization, and accessible
to your signed-in account. Sources needing enrollment are listed separately;
enroll them and press `r` to refresh.

Saved sources are found again if their address changes, and remain available to
forget while offline. If an older agent falls back to discovery on the CLI's
network, the list says so; update that agent to scan from the device instead.

`--name` gives the pairing a friendly name; `--sensors` limits it to selected
sensor names. Either flag opens the SensorLink tab, unless `--sensorlink` names
a source to pair (see below). Use `--list` to print saved SensorLink pairings.

### SensorLink scripting

Scripts and agents pair without the tabs. Name one source by its name (case is
ignored), source asset ID, or principal URI:

```sh
wendy --json device pair --device <target> --sensorlink garage-cam --name "Garage camera"
wendy --json device pair --device <target> --list
wendy --json device pair --device <target> --forget "Garage camera"
```

`--sensorlink` searches only the selected device's scan and pairs at most one
source. When no source matches, or several do, it pairs nothing and lists the
sources found with their asset IDs or principal URIs. An exact asset ID or
principal URI selects its source even when another source has that name. It also refuses a source
that needs enrollment, one already paired, and one outside your signed-in
organizations. If the device's agent is too old to scan, update it with
`wendy device update`, or pair in the tabs, which then scan the CLI computer's
network.

Pairing returns `{ "pairing": {...} }`. Listing returns
`{ "pairings": [...], "supports_principal_pairing": true }`. Forgetting returns
`{ "forgotten": {...} }` with the removed pairing. Each pairing has
`source_asset_id`, `org_id`, `name`, `sensor_allowlist`, `connected`,
`transport`, and `source_principal_uri`; `sensor_allowlist` is `[]` for all
sensors. A new pairing reports `connected: false`; check `--list` later.

`--forget` matches saved pairings, including offline ones, by the pairing name
that `--list` shows (the `--name` given when pairing, not the source's
discovered name), source asset ID, or principal URI.
To rename a pairing or change its sensors, forget it and pair it again. Without
a terminal, or with `--json`, `wendy device pair` needs `--list`, `--sensorlink`,
`--forget`, or `--usb-arms`.

## IP cameras

Select a camera and press Enter to save its login. Replace the suggested `admin`
username with your camera's account and enter its password. Passwords are masked.
Inside the form, Tab switches fields, Enter advances or saves, and Esc cancels.
Press `f` to forget a camera and remove its saved login, including when offline.

See [network cameras](/docs/hardware/camera#ip-cameras) for streaming and
camera-specific commands.

## Unitree Go2 robots

Agree with the robot's operator before pairing: the connection uses a robot
client slot and remains active after you close the CLI. Arrange a handoff if a
mobile app or another controller already uses it.

```sh
wendy device pair --device my-wendy --unitree
```

1. Press `l` to sign in to your Unitree account. Use `--unitree-region cn` for an
   account registered in China; the default region is `global`.
2. Select your robot by its serial number. Press Enter to connect.
3. If discovery does not find its address, press `i` to enter it. You can also
   start with `--ip 192.168.1.42` and select the matching account robot.

The password is masked and is not saved. The Wendy device saves the selected
robot's connection key so it can reconnect after a restart. Keys are stored in a
private file, without encryption at rest. Forget the pairing with `f` to remove
that key. Sign in again and reconnect to refresh it. Pairings belong to the Wendy
device's organization; unenrollment removes them.

Saved offline robots stay listed. Press Enter to reconnect or `i` to change an
address. Use a trusted robot LAN, especially with older firmware that cannot
provide authenticated connections.

Pairing sends connection checks and heartbeats, not movement commands.
`connected` does not confirm camera streaming, motor readiness, or physical safety.

## USB robot arms

Connect the arm to the selected Mac or Linux Wendy device, then open its inventory:

```sh
wendy device pair --device <target> --usb-arms
```

Select the USB serial endpoint, press Enter, and confirm that it is an SO-101.
Press `f` to forget a saved arm or `r` to refresh after reconnecting it. On a Mac,
use the native Wendy Agent app; the Go agent on macOS does not support this inventory.

Pairing saves a configuration; it does not move, calibrate, or verify the arm.
USB adapter information cannot prove which arm is attached. Check the physical
connection yourself before confirming. A saved arm may be present, disconnected,
or ambiguous. Resolve ambiguous matches before pairing; Wendy does not choose
one automatically.

Adapters with a serial number can keep their binding when moved between USB ports.
Without a serial number, the binding follows the USB port, including a replacement
adapter. The interactive prompt explains this; scripts must explicitly accept it
with `--allow-location-binding`.

### USB scripting

List the inventory, then copy the exact `id` into a bind or forget command:

```sh
wendy --json device pair --device <target> --usb-arms --list
wendy --json device pair --device <target> --usb-arms --bind '<id>' --arm-name Follower
wendy --json device pair --device <target> --usb-arms --forget '<id>'
```

Listing returns `{ "devices": [...] }`, binding returns `{ "device": {...} }`,
and forgetting returns `{ "forgotten": "<id>" }`. Field names use snake_case.
`configured` means a binding was saved; it does not mean the arm was verified.
`access_status` remains `unknown` because discovery does not open the serial port.

Bindings survive agent restarts. If the saved configuration is corrupt, Wendy
reports an error and preserves the file; ask your administrator to back it up
before repair. An unsupported agent reports that it needs updating rather than
scanning the CLI computer instead.

## Selection flags

| Flag | Purpose |
| --- | --- |
| `--list` | List SensorLink pairings, or USB inventory with `--usb-arms`. |
| `--sensorlink` | Pair one SensorLink source by name, asset ID, or principal URI. |
| `--name`, `--sensors` | Name or filter a SensorLink pairing. |
| `--unitree` | Open the Unitree tab. |
| `--ip` | Supply a Go2's IPv4 address. Opens the Unitree tab. |
| `--unitree-region` | Unitree account region, `global` or `cn`. |
| `--usb-arms` | Open or script USB arm setup. |
| `--bind` | Save an exact USB inventory ID. |
| `--forget` | Remove a SensorLink pairing, or a USB binding with `--usb-arms`. |
| `--arm-name` | Friendly USB arm name, up to 128 UTF-8 bytes. |
| `--allow-location-binding` | Accept a USB-port-based identity in scripts. |

Do not combine SensorLink, Unitree, and USB-specific flags in one operation.
