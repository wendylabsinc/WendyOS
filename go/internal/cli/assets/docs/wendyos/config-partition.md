# Config Partition

Every WendyOS image includes a small FAT32 partition labelled `config`, mounted at `/config` on the device. It is readable and writable on Linux, macOS, and Windows without any additional drivers or tools — including immediately after `dd`-ing an image onto an SD card or NVMe drive.

## Why it exists

The root filesystem on a WendyOS device is replaced wholesale during OTA updates. Anything written to `/` will be overwritten when an update is applied.

The `config` partition is outside the update boundary. Files placed there survive OS updates and are available from the very first boot. This makes it the right place for anything that needs to be pre-staged before a device has ever booted, or preserved across updates.

## Accessing it from a host computer

Because the partition is FAT32, your computer mounts it automatically when you plug in the storage.

- **macOS / Linux:** appears as a volume labelled `config`. On macOS, `wendy install` writes provisioning through this auto-mount directly.
- **Windows:** appears as a drive labelled `config`

Write files there, eject, and they are available at `/config` on the device on next boot.

This applies to the boards that install from a disk image (Raspberry Pi, x86). The boards flashed over USB never present their storage to the host, so `wendy install` writes a FAT32 image instead and programs it like any other partition:

| Board | How the partition is filled at install time |
| --- | --- |
| Raspberry Pi, x86 | Mounted on the host after the image is written |
| Jetson Thor, Orin | The flashpack's `config-partition.fat32.img` is injected, then flashed |
| Dragonwing IQ-8275, IQ-9075 | The bundle ships no config image, so wendy builds one on the host and programs it over EDL |

On Dragonwing the partition is created by a first-boot service rather than by the image build, so it stands down when it already finds a FAT32 filesystem labelled `config` — otherwise it would reformat the seed away.

## Accessing it on the device

The partition is mounted at `/config` on every boot (fstab `nofail`, so a missing or unformatted partition is not fatal):

```sh
ls /config
```

## wendy-agent self-update

If a file named `wendy-agent` is present in `/config` on boot, the agent validates it (must be a 64-bit ELF binary for the device's architecture) and, if valid, installs it over **the currently running agent binary** — `/opt/wendyos/bin/wendy-agent` on a stock image — then exits so systemd restarts it with the new binary. The file is deleted from `/config` regardless of outcome, so it is only applied once.

The install is refused if the target directory sits on a merged systemd-sysext overlay. A binary written there is discarded when the overlay is rebuilt on the next boot, so the seed would appear to succeed, silently do nothing, and retry forever.

`wendy install` writes the latest stable arm64 agent binary to the config partition automatically after flashing. If this write fails, `wendy install` warns rather than aborting (unless WiFi, device-name, or pre-enroll provisioning was explicitly requested — then the install exits non-zero, see [wendy.conf](#wendyconf)) — the device still boots using the agent baked into the image and fetches updates after first boot.

## Driver add-ons (`extensions.json`)

A file named `extensions.json` in `/config` seeds kernel driver add-ons at first boot,
so a device can come up with an out-of-tree driver it needs to reach the network at
all. It is a JSON array:

```json
[
  {
    "name": "your-driver",
    "artifact_url": "https://example.com/your-driver.raw",
    "kernel_version": "6.18.33-v8-16k",
    "sha256": "9668b870...",
    "signature": "<base64 detached signature>",
    "modules_load": ["your_driver"]
  }
]
```

Only `name` and `artifact_url` are required. `modules_load` overrides the module list
the image declares for itself, and is normally omitted.

The file is **deleted before any entry is applied**, not after. A bad or unreachable
URL therefore cannot wedge every subsequent boot, and a power cut mid-fetch leaves
nothing to retry. The whole batch is bounded at 90 seconds, of which up to 15 are spent
waiting for the network link, so a slow registry cannot stall startup. Anything dropped
is recoverable afterwards with `wendy device drivers install`.

> **Seeding is inert until a signing key ships.** Unlike a CLI install, the seed path
> requires a verified signature and has no operator to accept the risk. The embedded
> key is currently an empty placeholder, so every entry is refused with
> `cannot be authenticated (no signing key embedded)`. See
> [Managing drivers](/docs/device/managing-drivers).

## wendy.conf

`wendy.conf` is an INI-format file the agent reads on first boot to configure the device. If present, the agent applies its contents and then **deletes the file** so settings are not re-applied on subsequent boots.

### Format

```ini
[wifi]
ssid = MyNetwork
password = hunter2
```

The `[wifi]` section connects the device to a WiFi network. `password` may be omitted for open networks.

### How it is applied

On every boot, `wendy-agent` checks for `/config/wendy.conf`. If found:

1. If `[wifi]` contains a non-empty `ssid`, runs `nmcli device wifi connect <ssid> [password <password>]` to connect and create a persistent NetworkManager profile.
2. Deletes `/config/wendy.conf` — even on failure, so bad credentials are not retried on every boot.

`wendy install` writes this file automatically when you supply WiFi credentials (via `--wifi-ssid` / `--wifi-password` flags or the interactive prompt). If writing fails, `wendy install` warns and (on an interactive terminal) offers to retry. If the write never succeeds and you explicitly requested WiFi or device-name configuration, the install exits non-zero — the OS image itself is already on the drive, and the error message says so, so you can re-run `wendy install` or configure the device after it boots.

## Size

The partition is **64 MB** by default. This is intentionally small — it is for configuration, not application data or logs. Large or frequently-written data belongs on the data partition (`/data`).

The size can be adjusted at build time via `WENDYOS_CONFIG_PART_SIZE_MB` in your distro or `local.conf`.
