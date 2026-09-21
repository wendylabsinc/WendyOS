# `wendy device pair`

Discover and manage paired devices in the `Bluetooth | SensorLink | IP Cameras`
tabs. Use `Tab` or `Shift+Tab` to switch tabs. Opening a tab refreshes its devices.

```sh
wendy device pair
```

| Key | Action |
|-----|--------|
| `↑` / `↓` | Select a device |
| `Tab` / `Shift+Tab` | Switch tabs |
| `Enter` | Pair a device, or edit an IP camera's login |
| `f` | Forget the selected device |
| `r` | Refresh discovery |
| `q` / `Esc` | Quit |

Bluetooth scans on the connected WendyOS device. Unnamed peripherals are hidden
by default; `h` shows or hides them. `Enter` connects and pairs, and `d`
disconnects the selected peripheral.

SensorLink discovers sources on the CLI's local network. Devices must belong to
an organization you are logged into. Saved pairings remain available to forget
when their sources are offline.

IP cameras are discovered on the connected device's network. `Enter` opens a
username and password form that saves the camera's login on the device. Passwords
are masked. Inside the form, `Tab` switches fields, `Enter` advances or saves, and
`Esc` returns to the camera list. Saved offline cameras can be forgotten with `f`,
which removes the camera and its stored login.

| Flag | Description |
|------|-------------|
| `--list` | Print current SensorLink pairings |
| `--name` | Set a friendly name for a SensorLink pairing |
| `--sensors` | Limit a SensorLink pairing to these sensor names |

Passing `--name` or `--sensors` opens the SensorLink tab first.

The camera username starts with `admin` as an editable suggestion. Enter the
account configured on your camera and replace any factory-default password
before pairing it.
