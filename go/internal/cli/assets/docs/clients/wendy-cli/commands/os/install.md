# `wendy install`

Installs WendyOS onto an NVMe or SD card, fully recovers supported Jetsons over USB, or flashes Wendy Lite firmware onto an ESP32.

> **Tip:** [`wendy install`](../install.md) is the recommended, surfaced entry point for this command. `wendy os install` remains available and behaves identically — it is kept for backward compatibility and for discoverability under the `wendy os` group.

The command presents a unified device picker that lists Linux targets (Raspberry Pi, Jetson, ...) and ESP32 targets (C5, C6, C61, P4, S3). Select the device type to take the appropriate path:

- **Jetson Orin Nano / AGX Orin** -> download a recovery flashpack -> verify the module/carrier -> update QSPI and NVMe/eMMC together
- **Raspberry Pi targets** -> download OS image -> write to SD/NVMe -> write config partition
- **Jetson AGX Thor** -> download flashpack -> boot over USB recovery -> flash QSPI and internal NVMe
- **ESP32 targets** → detect USB serial port → download firmware `.bin` → flash over serial

```sh
# Interactive (recommended)
wendy install

# Install nightly firmware
wendy install --nightly

# Linux: non-interactive with all flags
wendy install --device-type raspberry-pi-5 --version 0.10.4 --drive /dev/disk4 --force

# Jetson AGX Thor: flash over USB recovery (macOS, Linux, and Windows)
wendy install --device-type jetson-agx-thor

# Orin Nano: full QSPI + NVMe recovery (macOS or Linux)
wendy install --device-type jetson-orin-nano

# AGX Orin: storage is mandatory non-interactively
wendy install --device-type jetson-agx-orin --storage nvme
wendy install --device-type jetson-agx-orin --storage emmc

# Explicit legacy raw-media write; QSPI is not updated
wendy install --device-type jetson-orin-nano --rootfs-only --drive /dev/disk4

# Direct install from a local image (Linux only)
wendy install path/to/image.img /dev/disk4 --force
```

> **Note:** `--device-type` is not supported for ESP32 targets. Use the interactive picker to flash an ESP32.

---

## Install a pull-request build

```sh
wendy install --pr 123
```

Downloads and flashes the WendyOS image built by wendyos-builder PR #123.
PR images are **debug builds**: SSH is enabled, root login is passwordless,
and the serial console is active. They are for testing the PR on hardware —
**never flash a PR image to a production device.** Artifacts are deleted when
the PR is closed.

`--pr` is supported for Linux disk-image devices, for Jetson recovery (Orin
Nano/AGX and AGX Thor) and for the Dragonwing IQ-8275 EDL flash. PR builds publish recovery flashpacks into the
`pr/<N>/` sandbox, so `--pr` can drive a full recovery install (QSPI+storage for
Orin, QSPI+NVMe for Thor) as well as `--pr --rootfs-only` raw imaging on Orin. It
is not supported for ESP32 targets (Wendy Lite firmware is not built by the
per-PR pipeline).
`--pr` is mutually exclusive with `--nightly`, `--version`, and a positional
image path.

---

## ESP32 (Wendy Lite) path

### 1. Device detection

The CLI scans for either the ESP32's native USB Serial/JTAG interface or the Silicon Labs CP210x USB-to-UART bridge used on many Espressif development boards:

| Platform | Expected path |
|----------|---------------|
| macOS | `/dev/cu.usbmodem*` or `/dev/cu.usbserial*` |
| Linux | `/dev/ttyACM0` or `/dev/ttyUSB0` (typical) |
| Windows | `COMN` (e.g. `COM7`) |

When both transports are connected, the installer always prefers native USB. A CP210x ID identifies a generic UART adapter rather than a confirmed ESP32, so the CLI treats it as an install candidate and verifies the chip model through the ROM bootloader before writing firmware.

UART is a flashing transport only: Wendy Lite's runtime USB connection uses the ESP32's native USB Serial/JTAG interface. The installer warns when it selects UART and, when no Wi-Fi network is configured in an interactive install, requires explicit confirmation before continuing.

If no device is found, the CLI prints instructions for entering bootloader mode:

```
No ESP32 device detected.
Make sure your ESP32 is connected via USB and in bootloader mode.
To enter bootloader mode: hold the BOOT button, press RESET, then release BOOT.
```

### 2. Firmware resolution

Firmware versions are served from the same manifest used for WendyOS images. With `--nightly`, the latest nightly build is used instead of the latest stable.

Boards without published firmware are omitted from the picker. Immediately before any chip-specific initialization or flash erase, the ROM bootloader's detected chip model is checked against the selected board; a mismatch is rejected before firmware is written. Wendy Lite currently supports ESP32-C5, C6, C61, P4, and S3 targets.

### 3. Serial flash protocol

The CLI implements the ESP32 ROM bootloader protocol directly over the USB serial port — no `esptool` dependency required. It supports both connection styles (a chip's native USB port and a CP210x USB-to-UART bridge), selecting the correct reset sequence for each. Progress is reported as blocks written / total blocks in the terminal.

### 4. Post-flash

The device reboots automatically using the reset sequence appropriate to its native USB or UART transport. Before flashing, the CLI embeds the selected WiFi credentials, device name, and pre-enrollment state into the firmware image's `wendy_conf` partition.

To provision WiFi after first boot, use `wendy device setup` or the BLE provisioning flow — see [BLE connectivity](../../../../wendy-agent/connectivity/ble.md).

---

## Linux (WendyOS) path

For Raspberry Pi devices—and Orin with `--rootfs-only`, the interactive flash-mode choice "OS image only", or a version that predates recovery flashpacks—the install path writes a disk image to a selected SD card, NVMe drive, or USB-attached enclosure:

1. **Resolve version** — `--version` if provided, otherwise latest (or nightly with `--nightly`).
2. **Resolve drive** — `--drive` if provided, otherwise an interactive picker of external drives. Internal drives require `--yes-overwrite-internal` in non-interactive mode; in interactive mode the user must type the device path to confirm.
3. **Download image** — fetched from GCS with a progress bar. Downloaded to `~/Library/Caches/wendy/os-images/` (macOS) or `~/.cache/wendy/os-images/` (Linux). Zip archives are streamed through to the first `.img`, `.raw`, `.wic`, or `.sdimg` entry; gzip-compressed images (`.img.gz`, detected by magic bytes regardless of extension) are decompressed and streamed on the fly. Seekable-zstd images (`.img.zst`) are downloaded and cached directly; when a block map is present, only mapped ranges are decoded during the write step, skipping hole frames entirely. Parallel download is used when the server supports HTTP range requests.
4. **Write image** — `dd`-equivalent write with elevated privileges (`sudo` on Unix, UAC on Windows), progress bar. When a block map is used and the bmap write fails (e.g. checksum mismatch or a stale/incorrect published bmap), the CLI automatically falls back to a full sequential write using the already-cached `.img.zst` or `.zip` — no re-download is required. A failure *during* the fallback write is fatal.
5. **Write config partition** — downloads the latest stable `wendy-agent-linux-arm64` binary from GitHub, writes it along with any pre-seeded WiFi credentials and device name to the config partition on the newly written drive. Skipped silently on platforms that don't support config-partition writes. The OS image is already on the drive by this point, and the device boots regardless of this step — it runs the agent baked into the image and fetches updates and configuration after first boot. When provisioning was **not** explicitly requested, a failure here is advisory: a warning is printed and the install is still reported as successful. When `--wifi`, `--device-name`, or `--pre-enroll` **was** requested and could not be applied, the install exits non-zero (see the exit-code note below). On an interactive terminal the CLI offers to retry the write after each failed attempt (useful after, e.g., re-seating an SD card whose config partition couldn't be located); non-interactively it prints guidance and does not retry.
6. **Eject** — the drive is ejected automatically after writing.

> **Exit code:** `wendy install` exits `0` when the OS image was written to the drive and any explicitly requested provisioning (`--wifi`, `--device-name`, `--pre-enroll`) was applied. It exits non-zero when the image itself could not be written, or when provisioning was explicitly requested but could not be applied — in the latter case the drive is still ejected first and the OS image is already safely on it, and the error names the image, version, and drive to make that clear, so the values can be re-applied with another `wendy install` or configured after the device boots. Unrequested provisioning (e.g. a failed agent-binary download when none of those flags were given) remains advisory: a warning is printed and the command exits `0`.

> **Provisioning retry:** When the config-partition write fails on an interactive terminal, the CLI asks `Retry writing provisioning data to the config partition?` after each attempt. Answering yes re-attempts the write (download + config-partition write); answering no, or running non-interactively, prints guidance and stops retrying — the command then exits non-zero if provisioning was explicitly requested (`--wifi`, `--device-name`, `--pre-enroll`), and successfully otherwise. The OS image is already on the drive either way.

## Jetson Orin full recovery path

New Orin releases default to full USB recovery on macOS, Linux, and Windows. Supported hardware is intentionally exact:

- Orin Nano P3767-0005 on P3768-0000, NVMe.
- AGX Orin P3701-0005 on P3737-0000, NVMe or eMMC.

The CLI RCM-boots a signed recovery initrd, correlates its mass-storage LUNs to the selected physical USB port and session, and reads `device.json` before any persistent write. A module/carrier mismatch aborts before the flash-package handoff. It then writes/ejects the flash package, writes the exported `nvme0n1` or `mmcblk0` according to the signed partition layout, collects device logs, and reports success only when the final status is `SUCCESS`.

Full recovery erases QSPI and every partition on the chosen storage, including `/data`. After the handoff, the first Ctrl+C warns that the device may be partially written; a second Ctrl+C confirms the abort. On Windows, the first flash installs a WinUSB driver for the Jetson recovery device and raw disk writes require elevation — expect a single administrator (UAC) prompt as soon as the flash mode is settled for a Jetson Orin target (answered at the interactive flash-mode question, or pinned by `--rootfs-only`/`--storage emmc`/a non-interactive run); accepting it continues the command, including the remaining setup questions, in a new elevated console window. If Windows offers to format one of the Jetson's flashing disks mid-flash, always choose Cancel.

`--drive`, `--no-bmap`, and `--yes-overwrite-internal` apply only with rootfs-only imaging. eMMC has no rootfs-only mode. Rootfs-only emits a warning because it does not update QSPI. Versions that predate recovery flashpacks fall back to their legacy SD/NVMe image automatically (with a warning; `--rootfs-only=false` turns the fallback into an error); a recovery-capable flash never falls back to raw imaging on failure.

## Jetson AGX Thor recovery flash path

Jetson AGX Thor does not use the drive-writing flow. Selecting `jetson-agx-thor` downloads the Thor flashpack, asks you to put the board into USB recovery mode, scans for the recovery-mode Jetson, then performs:

1. **Stage 1 RCM boot** — sends the Thor recovery payload over USB.
2. **Stage 2 partition flash** — flashes QSPI and the internal NVMe through the Thor flashing gadget. Expect around 25 minutes: USB transfers and device-side writes are deliberately serialized (concurrent USB access could crash the flash tooling, most notably on macOS), so this stage does not parallelize.
3. **Power-cycle** — after a successful flash, power-cycle the Thor out of recovery mode to boot WendyOS.

The CLI prompts for confirmation before erasing the Thor. No external USB drive is selected, and `--drive` does not apply to this path. On Windows, installing or updating the USB driver requires administrator approval (UAC).

### Stage 2 flash errors and recovery

A Stage 2 failure can leave the Thor booting only into the UEFI shell; the CLI prints a recovery guide when that is possible. In all of the cases below, the fix ends the same way: power-cycle the Thor back into USB recovery mode and re-run `wendy install`.

| Error | Meaning |
|---|---|
| `No flash progress for 15m0s — assuming bootburn is stuck and aborting it.` | The stall watchdog killed a flash that moved no data and logged nothing for 15 minutes (a wedged flash would otherwise hang forever). |
| `the wendy flash tooling crashed mid-write` | A flash helper process crashed; the full crash report is in the flash log. |
| `a device-side write command failed mid-flash` | A write on the Thor itself failed (bad image, full or failing NVMe) — check the flash log's `Command failed` line for the specific cause before retrying. |
| `USB access denied opening the flashing gadget` | Linux: install the wendy udev rule (USB vendor 0955) or run with sudo. macOS: quit whatever holds the gadget (e.g. `adb kill-server`). |

Every failure prints the path of the full flash log (`thor-flash-<timestamp>.log`), which contains the complete tooling output.

## Dragonwing IQ-8275 path

```sh
wendy install --device-type dragonwing-iq-8275
```

Connect the USB0 (USB-C) port, power off, set DIP switch 3 ON, and power on. Wendy downloads and verifies the bundle, and programs the board. Set DIP switch 3 OFF and power-cycle after success.

Both OS slots and the partition table are rewritten. Existing configuration and data are preserved when flashing a compatible WendyOS layout.

## Linux Desktop / Headless Mac path

Choosing **Linux Desktop** or **Headless Mac** (or `--device-type linux-desktop` / `--device-type headless-mac`) does not write a drive. The command prints the `agent.sh` one-liner from the [Linux installation guide](/docs/installation/linux) (the script auto-detects the target platform). When `--pre-enroll` is supplied and a valid auth session exists, a 1-hour enrollment token is embedded in the command; the agent self-enrolls on first startup without a separate `wendy device enroll` step. Run `wendy discover` afterwards to find the device.

### Privileges

Thor flashing talks to the board's USB recovery device directly (an in-process libusb handle), so — unlike the SD/NVMe disk-image path, which shells out to `sudo` only for the disk write — the **whole command must run as root**.

`wendy install` handles this for you: when it is not already running as root it re-executes itself under `sudo` **before** the recovery briefing, so you are prompted for your password up front rather than hitting a permission error partway through the flash. The elevated run reuses the already-downloaded flashpack (no re-download) and skips straight to the Thor flow.

- **macOS** — always elevates when not run as root; the OS binds its own driver to the recovery device, so there is no non-root path.
- **Linux** — if the wendy udev rule (`70-wendy-jetson.rules`, installed by the deb/rpm package or `wendy device usb-setup`) is present, the flash runs as your user with **no prompt**. Otherwise it re-execs under `sudo`.
- **Non-interactive** (CI, piped input) — the CLI cannot prompt for a password, so it exits with instructions to re-run under `sudo` (Linux: or install the udev rule) instead of hanging.

### WiFi pre-configuration

```sh
# Single network
wendy install --wifi-ssid MyNetwork --wifi-password hunter2

# Multiple networks, highest-priority first
wendy install \
  --wifi "ssid=Home,password=hunter2,priority=100" \
  --wifi "ssid=Office,password=corp,priority=50" \
  --wifi "ssid=Cafe,hidden=true"

# Skip WiFi setup entirely
wendy install --no-wifi
```

`--wifi-ssid` without `--wifi-password` checks the system keychain (macOS) first, then prompts. In interactive mode without any `--wifi` flags, the CLI asks whether to configure WiFi and offers to scan nearby networks. If the scan fails or finds no networks, you can choose to enter credentials manually or skip WiFi setup entirely (configure later with `wendy device wifi connect`).

> **Note:** When a password is entered at an interactive prompt, the input is masked — characters appear as `•` so the password is never displayed in plaintext on screen.

The `--wifi` flag accepts `key=value` pairs separated by commas. Keys: `ssid` (required), `password`/`pass`/`psk`, `priority` (integer), `hidden` (true/false), `security` (e.g. `wpa2`). Commas inside values can be escaped with `\,`.

### Pre-enrollment

```sh
wendy install --pre-enroll
```

Requires an active `wendy auth login` session. The CLI creates an enrollment token via Wendy Cloud and writes provisioning JSON to the config partition so the device enrolls and receives mTLS certificates on first boot. Without `--pre-enroll`, the device boots unenrolled and can be enrolled later with `wendy device enroll`.

### Flags reference

| Flag | Default | Description |
|------|---------|-------------|
| `--nightly` | false | Use nightly/pre-release builds |
| `--pr` | — | Install from wendyos-builder PR #N (mutually exclusive with `--nightly`, `--version`, positional path; not supported for ESP32 targets) |
| `--device-type` | — | Device type from manifest (Linux targets only, e.g. `raspberry-pi-5`; not supported for ESP32 targets: `esp32-c5`, `esp32-c6`, `esp32-c61`, `esp32-p4`, `esp32-s3`) |
| `--version` | latest | WendyOS version to install (Linux only) |
| `--drive` | interactive | Target drive path (e.g. `/dev/disk4`) |
| `--rootfs-only` | false | Explicitly write only an Orin SD/NVMe image; QSPI is not updated |
| `--force` | false | Skip confirmation prompts |
| `--yes-overwrite-internal` | false | Required to wipe a non-removable drive non-interactively |
| `--wifi-ssid` | — | Pre-configure a single WiFi network |
| `--wifi-password` | — | Password for `--wifi-ssid` |
| `--wifi` | — | Pre-configure one WiFi network; repeatable |
| `--no-wifi` | false | Skip WiFi setup entirely |
| `--device-name` | interactive | Set device name on first boot (lowercase letters, digits, hyphens; must start with a letter, 3–55 chars) |
| `--pre-enroll` | auto | Pre-enroll with Wendy Cloud. For WendyOS/Jetson targets, embeds a certificate during imaging. For Linux Desktop / Headless Mac, mints a short-lived token printed with the `agent.sh` one-liner. |
| `--cloud-grpc` | auto | Cloud gRPC endpoint of the auth session to use for pre-enrollment (applies to both drive imaging and Linux Desktop / Headless Mac); defaults to the session set via `wendy auth use`. |
| `--storage` | auto | Storage variant: `nvme`/`sd` for raw imaging; AGX Orin recovery requires `nvme` or `emmc` non-interactively |
| `--no-bmap` | false | Disable bmap-accelerated flashing even when a block map is available |

> **TODO**: Post-flashing Linux devices still need certificate provisioning and Wendy Cloud enrollment if `--pre-enroll` was not used. See [`wendy device setup`](../device/setup.md), [PKI](../../../../pki/), and [Wendy Cloud](../../../../cloud/).
