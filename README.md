# Wendy

Build and run apps on robots, Raspberry Pis, Jetsons, and other Linux devices.
Wendy builds your code, deploys it, and brings the logs back to your terminal.

The CLI runs on your development computer. The Wendy Agent runs on the device
and manages your apps. WendyOS includes the Agent. You can also install the
Agent on an existing Linux computer or robot and keep its OS.

[Documentation](https://docs.wendy.dev) · [Examples](Examples) · [Discord](https://discord.gg/wendylabsinc)

## Why Wendy?

Getting a container to start is only part of building a robot or edge app.
You also need to build for the device, transfer changes, configure hardware
access, and find out why the app failed. Wendy connects those steps in one
workflow, so you spend less time maintaining deployment scripts.

| If you use... | What Wendy adds |
| --- | --- |
| Docker or Compose | `wendy run` builds for your target, transfers the app, starts it, and attaches logs. `wendy run --watch` repeats the loop after edits. Bring your Dockerfile or a supported Compose project. |
| Ubuntu or JetPack | Install Wendy Agent to get deployment and inspection tools while keeping your OS. Choose WendyOS on supported boards for a supplied OS image and its A/B OS update path. |
| Separate hardware and robotics tools | Declare hardware permissions in `wendy.json`, inspect ROS 2 nodes and topics, and connect a coding agent through MCP. |

Local development needs no Wendy Cloud account. Wendy Cloud adds remote access
and fleet management. Installing the Agent on Ubuntu does not add WendyOS OS updates.

[Compare Wendy with Docker, Ubuntu, balena, and OTA tools](https://docs.wendy.dev/latest/concepts/why-wendy/)
and see where each is a good fit.

## Install

On macOS or Linux:

```sh
curl -fsSL https://install.wendy.dev/cli.sh | bash
wendy --version
```


On Windows:

```powershell
winget install WendyLabs.Wendy --source winget
```

[Other installation methods and updates](INSTALL.md)

## Choose a starting point

### Use your own device

```sh
wendy tour --hardware
```

The tour guides you through device setup. Start with a Raspberry Pi, NVIDIA
Jetson, or an existing Linux computer or robot.
[Find the setup guide for your hardware](https://docs.wendy.dev/latest/installation/).

### Try a simulated robot

```sh
wendy tour --simulator
```

The tour creates a simulated Unitree Go2, opens its 3D viewer, and runs a short
walking demo. No physical robot or cloud account is needed. The installer may
start the tour automatically.

You need macOS or Linux, Docker with Buildx, and QEMU. Allow 4 GiB of memory for
the VM and several minutes for the first download and build. Apple Silicon is
the fastest path; x86 Linux uses ARM64 emulation.
See [simulator setup](https://docs.wendy.dev/latest/installation/wendyos-virtual-machine/)
for prerequisites. When finished, use the `wendy vm stop` command printed by the tour.

## Build and deploy an app

With your device set up and Docker running, create a small HTTP app:

```sh
wendy init hello-wendy --target wendyos --language node --template simple-api --assistant skip --git-init no
cd hello-wendy
wendy run
```

Select your device. Open the address Wendy prints and check for
`{"message":"hello-world"}`. Edit `src/index.ts`, save, then run `wendy run` again
to rebuild and deploy your change. Node.js runs inside the container.

## Build with a coding agent

After your first successful run, you can let a coding agent deploy changes and
inspect the device through Wendy's MCP server:

```sh
wendy mcp setup
```

Follow the [coding-agent setup guide](https://docs.wendy.dev/latest/guides/llm-integration/)
for supported tools. For ChatGPT, see the [ChatGPT guide](https://docs.wendy.dev/latest/guides/chatgpt/).


## Keep building

- [Robot apps](https://docs.wendy.dev/latest/integrations/ros2/) with ROS 2 in containers, remote topic inspection, and a Foxglove bridge.
- [Camera and AI apps](https://docs.wendy.dev/latest/hardware/) with access to cameras, audio, and GPUs.
- [Multi-service apps](https://docs.wendy.dev/latest/guides/stagefile/) with incremental builds and automatic redeployment through `wendy run --watch`.
- [Data collection](https://docs.wendy.dev/latest/data/) that records sensor data, app events, and model results around triggers.

Browse the [examples](Examples) or follow a
[tutorial](https://docs.wendy.dev/latest/guides/tutorials/) to build your next app.
[Wendy Cloud](https://cloud.wendy.dev) adds remote access and fleet management.
Local development does not require a cloud account.
