Wendy Labs is the infrastructure for Physical AI, targeting Edge (AI) devices such as robotics, IoT and satellites.

### Problem

Before Wendy, it's very hard to install, develop and deploy these devices.

- Setting up the OS, drivers and tooling is manual, and requires peripherals such as a keyboard, mouse and monitor
- Running on a device is a custom effort, whether through rsync, zip files, or containers
- Scaling your solution to many devices is not standardised either

In cloud services, all of these are solved through pre-baked images and container infrastructure. Wendy brings that to the edge.

### Solution

Wendy Labs has three main components to achieve this mission:
- WendyOS
- Wendy-Agent
- Cloud

[WendyOS](/docs/#2-set-up-a-device) is a pre-built OS for specific hardware (NVIDIA Jetson — Orin Nano, AGX Orin, AGX Thor —, Raspberry Pi, ...) that packages all the tools you need to build & run apps.

Wendy-Agent is a daemon, akin to Docker, that manages and runs your apps. It also exposes a way to remotely control the machine and upload apps.

[Wendy Cloud](https://cloud.wendy.dev/login) is a tool for managing your Wendy devices, viewing (crash) logs, provisioning updates and more.

### Agent & CLI

The Agent comes in two flavours, Linux and macOS, for running apps on these platforms respectively.

The [Wendy CLI](/docs/reference/cli) enables your developers to manage these devices, locally through LAN, BLE or over USB.

The CLI also manages enrolled devices through Wendy Cloud, including deployment
and remote access.

## Wendy Net routing

Wendy Net can carry traffic through intermediate devices when the destination is
not a direct neighbor. Its routing engine selects paths independently of whether
the links use Ethernet, Wi-Fi, or Bluetooth.

For contributors, the engine is a separate Go library in `go/babel`. It uses the
Babel routing protocol; carrier integration and application services belong to
the agent. See the [library guide](https://github.com/wendylabsinc/WendyOS/blob/main/go/babel/README.md)
for its API and the [testing guide](../development/testing.md#routing-engine-tests)
for the separate module checks.
