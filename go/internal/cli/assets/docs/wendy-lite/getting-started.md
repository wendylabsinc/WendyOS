# Getting Started with Wendy Lite

Wendy Lite is a runtime for Espressif ESP32 microcontrollers. It runs
on your device, handles its connectivity, and lets you build, deploy and run
your own native applications on it — all from a single command-line tool, the
Wendy CLI.

This guide takes the shortest path from nothing to a running application: three
steps, ending with an existing example built and deployed on your device.

## Step 1 — Install the tools

You need two tools.

**Wendy CLI** — your entry point to the device: installs Wendy Lite, configures
it, builds and deploys applications. Run this command to install Wendy CLI:

```sh
curl -fsSL https://install.wendy.dev/cli.sh | bash
```

or follow the
[CLI installation guide](https://docs.wendy.dev/latest/installation/developer-machine-setup/#cli-installation).

**EIM**, the ESP-IDF Installation Manager — Espressif's tool for managing the
ESP-IDF development environment. Follow Espressif's
[installation instructions](https://docs.espressif.com/projects/esp-idf/en/stable/esp32/get-started/index.html#installation).

You never have to select a toolchain by hand: the Wendy CLI picks the right
one for you via EIM.

## Step 2 — Install Wendy Lite on your device

Connect your device over USB and run:

```sh
wendy install
```

This flashes Wendy Lite onto the device. When prompted, select the board variant
labeled **native app support**; variants without that label cannot run the native
`blink-rgb-led` example. You only do this once per device.

During installation, `wendy install` also prompts you to configure the Wi-Fi
connection and enroll the device in Wendy Cloud — feel free to skip these
for now.

## Step 3 — Build and deploy an application

Rather than writing an application from scratch, start from an existing one.
`blink-rgb-led` is the simplest example: it blinks the onboard RGB LED.

Scaffold the project and enter its directory:

```sh
mkdir blink-rgb-led
cd blink-rgb-led
wendy init --template blink-rgb-led
```

Then build, deploy and run it:

```sh
wendy run
```

That is the whole cycle. `wendy run` compiles the project in the current
directory, pushes the application to the device, and starts it. The onboard LED
should start blinking.

`blink-rgb-led` runs on these development kits:

- ESP32-C5-DevKitC-1
- ESP32-C6-DevKitC-1
- ESP32-C6-DevKitM-1
- ESP32-C61-DevKitC-1

More native examples are in the
[wendy-lite-native-apps](https://github.com/wendylabsinc/wendy-lite-native-apps)
repository.

## Next steps

You now have a working setup and a project you can edit. Change something in
`blink-rgb-led/main/main.c`, run `wendy run` again, and watch it land on the device.

When you want to write your own application, see
[Native Apps for Wendy Lite](native-apps.md) — it covers how a Wendy
Lite project is put together, how to integrate the `wendy_core` component, and
which parts of the hardware the system owns.
