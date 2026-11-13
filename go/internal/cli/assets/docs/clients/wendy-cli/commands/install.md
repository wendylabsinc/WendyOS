# `wendy install`

Installs WendyOS onto an NVMe or SD card, fully recovers supported Jetsons over USB, flashes Wendy Lite firmware onto an ESP32, or prints `agent.sh` install instructions for an existing Linux machine or Mac.

`wendy install` is the surfaced, top-level alias for [`wendy os install`](./os/install.md). It is the recommended entry point: the underlying `wendy os` group is hidden from the main help, but the install flow is the most common first step when bringing up a device, so it is promoted to a first-class command.

The two commands are the **same command** — they accept identical flags and arguments and behave identically. `wendy os install` remains available for backward compatibility and for discoverability under the `wendy os` group.

The interactive picker has Linux, Mac, and Microcontrollers tabs. Press Tab or Shift+Tab to switch categories, use the arrow keys to choose a device, and press Enter to select it. Linux includes WendyOS devices and Linux Desktop; Mac offers Headless Mac; Microcontrollers lists ESP32 targets with published Wendy Lite firmware. Empty categories are hidden.

When supported USB flashing devices are connected, a Detected USB tab appears first. It recognizes Jetson AGX Thor, AGX Orin and Orin Nano recovery mode, Qualcomm EDL mode for Dragonwing and Arduino UNO Q, and ESP32-S31 ROM download mode for boards such as Mosaico. Qualcomm and ESP32-S31 devices still require a board-model choice because their USB IDs are shared. The scan reads USB descriptors without resetting or opening devices. Ordinary ESP32 serial interfaces and USB-UART bridges cannot prove flashing mode, so use the Microcontrollers tab for those. The installer selects the physical target again before flashing.

```sh
# Interactive (recommended)
wendy install

# Install nightly firmware
wendy install --nightly

# Download a matching driver and push it after the flashed device boots
wendy install --device-type dragonwing-iq-9075 --device-name iq9-mesh --nightly --driver intel-be202

# Linux: non-interactive with all flags
wendy install --device-type raspberry-pi-5 --version 0.10.4 --drive /dev/disk4 --force \
  --device-name lab-pi --wifi-ssid MyNetwork --wifi-password hunter2

# Same, for a board you reach over Ethernet or USB
wendy install --device-type raspberry-pi-5 --drive /dev/disk4 --force --no-wifi

# Prompts still work when you keep a log of the output
wendy install | tee install.log

# Use a locally built Linux agent instead of downloading the latest release
wendy install --device-type raspberry-pi-5 --binary ./wendy-agent-linux-arm64

# Direct install from a local image (Linux only)
wendy install path/to/image.img /dev/disk4 --force

# Install wendy-agent on an existing Linux machine or Mac (prints a one-liner)
wendy install --device-type linux-desktop
wendy install --device-type headless-mac

# Same, with pre-enrollment into your org (requires `wendy auth login`)
wendy install --device-type linux-desktop --pre-enroll
```

## Prompts, pipes and scripts

When you type `wendy install` in a terminal, it asks its questions even when its output goes to a pipe or a file, as in `wendy install | tee install.log`. The prompts are drawn on the terminal, together with the first-boot summary and the warning before each erase. The log keeps the rest of the output, including that summary and the flash steps.

Without a terminal (CI, an agent's shell), nothing is asked. Pass `--wifi` or `--wifi-ssid`, or `--no-wifi`; without one of them the install stops before anything is downloaded or erased. Pass `--force` for the confirmations. Without `--device-name`, the device gets an auto-generated name. See [Running without a terminal](os/install.md#running-without-a-terminal) for every flag a script needs.

Before anything is erased, the installer prints what first boot will apply, also with `--force`: the device name, the Wi-Fi networks and Cloud enrollment (Wendy Lite enrolls after flashing, so its summary has no Cloud line). Passwords are never printed.

## Reference

See [`wendy os install`](./os/install.md) for the complete reference: the ESP32 (Wendy Lite) flash path, the Linux (WendyOS) write path, WiFi pre-configuration, pre-enrollment, exit-code semantics, and the full flags table.

## Linux Desktop / Headless Mac

Selecting **Linux Desktop** or **Headless Mac** in the interactive picker (or passing `--device-type linux-desktop` / `--device-type headless-mac`) does not write any drive. Instead it prints a `curl … | bash` one-liner for `agent.sh`, which auto-detects the platform. The Linux command adds `--network-access network` so the machine can be discovered and managed from your computer; for a Mac, choose **Allow Other Devices** when Wendy Agent first opens. When you are logged in and confirm (or pass `--pre-enroll`), it mints a short-lived (1 h) enrollment token and embeds it in the command so the freshly installed `wendy-agent` self-enrolls on first startup. Without a token the device is discovered over mDNS as described in the [Linux installation guide](/docs/installation/linux).

## Related

- [`wendy os install`](./os/install.md) — full installation reference (same command)
- [`wendy os download`](./os/download.md) — pre-download a WendyOS image into the cache
- [`wendy device setup`](./device/setup.md) — provision a device after first boot
