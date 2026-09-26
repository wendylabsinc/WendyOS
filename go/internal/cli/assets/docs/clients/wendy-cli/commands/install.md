# `wendy install`

Installs WendyOS onto an NVMe or SD card, fully recovers supported Jetsons over USB, flashes Wendy Lite firmware onto an ESP32, or prints `agent.sh` install instructions for an existing Linux machine or Mac.

`wendy install` is the surfaced, top-level alias for [`wendy os install`](os/install.md). It is the recommended entry point: the underlying `wendy os` group is hidden from the main help, but the install flow is the most common first step when bringing up a device, so it is promoted to a first-class command.

The two commands are the **same command** — they accept identical flags and arguments and behave identically. `wendy os install` remains available for backward compatibility and for discoverability under the `wendy os` group.

The interactive picker has Linux, Mac, and Microcontrollers tabs. Press Tab or Shift+Tab to switch categories, use the arrow keys to choose a device, and press Enter to select it. Linux includes WendyOS devices and Linux Desktop; Mac offers Headless Mac; Microcontrollers lists ESP32 targets with published Wendy Lite firmware. Empty categories are hidden.

```sh
# Interactive (recommended)
wendy install

# Install nightly firmware
wendy install --nightly

# Linux: non-interactive with all flags
wendy install --device-type raspberry-pi-5 --version 0.10.4 --drive /dev/disk4 --force

# Direct install from a local image (Linux only)
wendy install path/to/image.img /dev/disk4 --force

# Install wendy-agent on an existing Linux machine or Mac (prints a one-liner)
wendy install --device-type linux-desktop
wendy install --device-type headless-mac

# Same, with pre-enrollment into your org (requires `wendy auth login`)
wendy install --device-type linux-desktop --pre-enroll
```

## Reference

See [`wendy os install`](os/install.md) for the complete reference: the ESP32 (Wendy Lite) flash path, the Linux (WendyOS) write path, WiFi pre-configuration, pre-enrollment, exit-code semantics, and the full flags table.

## Linux Desktop / Headless Mac

Selecting **Linux Desktop** or **Headless Mac** in the interactive picker (or passing `--device-type linux-desktop` / `--device-type headless-mac`) does not write any drive. Instead it prints a `curl … | bash` one-liner for `agent.sh` — the command is identical for both, the script auto-detects the platform. When you are logged in and confirm (or pass `--pre-enroll`), it mints a short-lived (1 h) enrollment token and embeds it in the command so the freshly installed `wendy-agent` self-enrolls on first startup. Without a token the device is discovered over mDNS as described in the [Linux installation guide](/docs/installation/linux).

## Related

- [`wendy os install`](os/install.md) — full installation reference (same command)
- [`wendy os download`](os/download.md) — pre-download a WendyOS image into the cache
- [`wendy device setup`](device/setup.md) — provision a device after first boot
