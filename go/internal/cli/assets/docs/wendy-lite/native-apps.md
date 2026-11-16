# Native Apps for Wendy Lite

## Overview

Wendy Lite is a runtime for Espressif ESP32 microcontrollers. It handles your
device's connectivity and lets you write your own **native applications**,
build them, and deploy them to it.

Native apps are built with Espressif's tooling, in particular
[ESP-IDF](https://docs.espressif.com/projects/esp-idf/), in the version of the
Wendy Lite build on your device (**6.0.3** for most boards, **6.1** for the
ESP32-S31).
ESP-IDF lets you write your application in C or C++. Because it is driven by
CMake, you can also integrate other compiled languages, such as Rust or Swift.

The **Wendy CLI** is your entry point to the device. It lets you:

- install Wendy Lite on a device,
- configure Wi-Fi and cloud access,
- build and deploy your native application.

Deployment can happen over three transports:

| Transport | Availability |
| --- | --- |
| USB | available |
| Wi-Fi (local network) | available |
| Cloud | coming soon |

Configuring a device can additionally go over **Bluetooth Low Energy (BLE)**.
BLE is there mainly to reach a device in the situations where neither Wi-Fi nor
USB is available.

## Prerequisites

Before you start, you need:

- **Wendy CLI** — installs Wendy Lite on a device, configures it, and builds
  and deploys your application.
- **EIM**, the ESP-IDF Installation Manager — Espressif's tool for managing the
  ESP-IDF development environment (toolchains, Python environment, SDK
  versions). The Wendy CLI manages this environment for you, so you never
  need to pin a version yourself: `wendy run` installs the ESP-IDF version of
  the Wendy Lite build on the device. If EIM is missing, `wendy run` offers to
  install it with Homebrew.
- a device already flashed with Wendy Lite.

[Getting Started with Wendy Lite](./getting-started.md) walks through
installing the CLI and flashing a device.

When you install Wendy Lite on your device, ensure you pick the variant
labeled **native app support**. If you are not sure whether your installed
version supports native apps, run `wendy device info` to check.

## Anatomy of a Wendy Lite native app

A Wendy Lite native app is an ordinary ESP-IDF project with two additions:

- a `wendy.json` manifest in the project root.
- the **`wendy_core`** component, at the Wendy Lite version of the device.
- a `sdkconfig.defaults.wendy` file: the configuration of the Wendy Lite build
  on the device.

`wendy_core` is what makes your application a Wendy app rather than a bare
ESP-IDF firmware. It runs your code inside the Wendy environment, so that the
device can be managed by the Wendy CLI and reached through Wendy Cloud.

Creating an app therefore comes down to three steps:

1. Create an ESP-IDF project.
2. Declare `wendy_core` in the project's dependencies.
3. Call `wendy_core_init()` at the very beginning of `app_main()`.

### Step 1 — Create an ESP-IDF project

Use the standard ESP-IDF project layout: a top-level `CMakeLists.txt`, a `main`
component, a `sdkconfig.defaults` holding your build configuration, and a
`wendy.json` manifest:

```text
my-app/
├── CMakeLists.txt
├── sdkconfig.defaults
├── sdkconfig.defaults.wendy
├── wendy.json
└── main/
    ├── CMakeLists.txt
    ├── idf_component.yml
    └── main.c
```

`wendy.json` looks like this:

```json
{
  "$schema": "https://wendy.dev/schemas/wendy.json",
  "appId": "com.example.my-esp32-app",
  "version": "0.1.0",
  "platform": "wendy-lite",
  "entitlements": []
}
```

> **`sdkconfig` is generated, not authored.**
> It's created on first build and can be regenerated whenever the target
> changes, so changes made directly to it can be lost. Store your
> configuration in `sdkconfig.defaults` instead, or, if you really want to
> rely on it, keep `sdkconfig` itself in your git repo.

A native app is a full firmware image, so it must be configured like the
Wendy Lite build on the device: flash size, WASM support, Wendy Cloud tunnel,
board options. `sdkconfig.defaults.wendy` holds that configuration. `wendy run`
generates it from the master configuration of the device's Wendy Lite version
and the device's build profile, and offers to update it when the device runs
another version or profile. Do not edit it, and commit it with your project.
The partition table lines of the firmware are left out of it.

The top-level `CMakeLists.txt` lists it in `SDKCONFIG_DEFAULTS`, before the
include of `project.cmake`. Your own `sdkconfig.defaults` comes last, so its
lines win:

```cmake
cmake_minimum_required(VERSION 3.16)

set(SDKCONFIG_DEFAULTS "sdkconfig.defaults.wendy;sdkconfig.defaults")
include($ENV{IDF_PATH}/tools/cmake/project.cmake)
project(my-app)
```

`wendy run` offers to add this line, and to create an empty
`sdkconfig.defaults` when there is none (ESP-IDF stops when a listed file is
missing). When `sdkconfig.defaults.wendy` or `CMakeLists.txt` changes,
`wendy run` generates `sdkconfig` again and keeps the previous one as
`sdkconfig.old`.

### Step 2 — Declare the `wendy_core` dependency

Add `wendy_core` to your component manifest, `main/idf_component.yml`. Its
`version` is the Wendy Lite version of the device, a git tag or commit of
wendy-lite:

```yaml
dependencies:
  idf:
    version: ">=5.5.0"
  wendy_core:
    git: "https://github.com/wendylabsinc/wendy-lite.git"
    path: "components/wendy_core"
    version: "<Wendy Lite version of the device>"
```

`wendy run` offers to add the dependency when it is missing, and to change its
`version` when the device runs another Wendy Lite version. A `wendy_core` from
a local `path`, an `override_path` or another repository is not checked.

Then require it from your component in `main/CMakeLists.txt`:

```cmake
idf_component_register(SRCS "main.c"
                    INCLUDE_DIRS "."
                    REQUIRES wendy_core)
```

### Step 3 — Initialize Wendy Core

`wendy_core_init()` must be the **first** function your application calls:

```c
#include "wendy_core.h"

void app_main(void)
{
    ESP_ERROR_CHECK(wendy_core_init());

    /* Your application starts here. */
}
```

This call brings up the agent that handles all communication with the Wendy
environment. Nothing else in your application may run before it.

## Starting from an example

The quickest way to start a project is to copy an existing one. The
[`blink-rgb`](https://github.com/wendylabsinc/wendy-lite-native-apps/tree/main/blink-rgb)
project is a deliberately minimal example: it shows
how a project for Wendy Lite is laid out and how `wendy_core` is integrated.

Its `main/main.c` calls `wendy_core_init()` first, as in Step 3 above, then
adds a simple blink loop:

```c
#include <stdio.h>
#include <stdbool.h>

#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "rgb_led.h"

#include "wendy_core.h"

#define RGB_LED_GPIO 8

void app_main(void)
{
    ESP_ERROR_CHECK(wendy_core_init());

    ESP_ERROR_CHECK(rgb_led_init(RGB_LED_GPIO, 1));

    bool on = false;
    while (true) {
        on = !on;
        if (on) {
            ESP_ERROR_CHECK(rgb_led_set(0, 24, 24, 0));
        } else {
            ESP_ERROR_CHECK(rgb_led_clear());
        }
        vTaskDelay(pdMS_TO_TICKS(500));
    }
}
```

## What Wendy Lite owns, and what your app must not touch

Wendy Lite handles communication for you. It owns and initializes the
communication controllers and peripherals:

- **USB**
- **Wi-Fi**
- **Bluetooth Low Energy (BLE)** — the system uses it above all to configure the
  device when Wi-Fi and USB are not available.

Your application is free to *use* these channels, but it must
never configure them itself. Do not initialize the Wi-Fi or BLE controller, and
do not set up the network stack: use what Wendy Lite has already configured.

> **Do not use the USB Serial/JTAG controller as a console.**
> Wendy Lite claims that peripheral and drives the console itself. Configuring
> it from your application, or using it for your own I/O, will conflict with
> the system.

## Building and deploying

One command covers the whole cycle. Run it from your project directory:

```sh
wendy run
```

`wendy run` compiles the project, pushes the resulting application to the
device, and starts it. It selects the ESP-IDF version of the device's Wendy
Lite build for you,
so you do not have to pin one yourself. The application is pushed over USB, over
the local network, or (soon) through the cloud.

To rebuild the project entirely from scratch, delete `build`, `sdkconfig`,
and `managed_components`, then run `wendy run` again. Also delete
`dependencies.lock` if you want to move to more recent versions of your
dependencies (such as `wendy_core`).

## The Project Remains a Normal ESP-IDF Project

Your application can use ESP-IDF components, managed components, Kconfig
options, and peripheral drivers, exactly as in any other ESP-IDF project —
except for the restrictions listed above. This is especially important for
displays, cameras, audio, and other hardware that needs full access to the
native ESP-IDF APIs.

By including a partition table that matches the one Wendy Lite itself uses
for the installed firmware variant, you can still compile and deploy through
classic IDF commands:

```bash
idf.py set-target esp32c6
idf.py menuconfig
idf.py build
idf.py -p /dev/cu.usbmodemXXXX flash monitor
```

There is no CLI command yet to fetch this partition table. Extract the
`partitions.csv` file that corresponds to your Wendy Lite variant from the
[wendy-lite repository](https://github.com/wendylabsinc/wendy-lite), add it
to your project, and enable it in `sdkconfig.defaults`:

```ini
CONFIG_PARTITION_TABLE_CUSTOM=y
CONFIG_PARTITION_TABLE_CUSTOM_FILENAME="partitions.csv"
```

Without this, `idf.py flash` uses the project's default partition table
instead, which can overwrite or omit Wendy Lite's reserved partitions.
