# WendyOS · Infrastructure for Physical AI

[![Discord](https://img.shields.io/badge/Discord-Join%20Server-5865F2?logo=discord&logoColor=white)](https://discord.gg/wendylabsinc)
[![Docs](https://img.shields.io/badge/Docs-Available-468900?logo=discord&logoColor=white)](https://docs.wendy.dev)
[![Blog](https://img.shields.io/badge/Blog-893366)](https://wendy.dev/blog)

Wendy brings Software Infrastructure to the **physical world**.
1. Deploy Rapidly
2. Scale Globally
3. Manage Remotely

### Deploy in Seconds

[![Unitree G1 and Go2](https://img.shields.io/badge/Unitree-G1%20&amp;%20Go2-0076B9?logo=&logoColor=white)](https://docs.wendy.dev/latest/installation/wendy-agent-unitree-g1/)
[![NVIDIA Jetson](https://img.shields.io/badge/NVIDIA-Jetson%20Nano/Orin/Thor-76B900?logo=nvidia&logoColor=white)](https://docs.wendy.dev/latest/installation/wendyos-nvidia-jetson-agx-thor/)
[![Raspberry Pi](https://img.shields.io/badge/Raspberry%20Pi-3,%204,%205-c51d4a?logo=raspberry-pi&logoColor=white)](https://docs.wendy.dev/latest/installation/wendyos-raspberry-pi-5/)
[![Linux x86 and arm64](https://img.shields.io/badge/Linux-any%20x86%20or%20arm64-888888?logo=linux&logoColor=white)](https://docs.wendy.dev/latest/installation/linux/)
[![Headless Mac](https://img.shields.io/badge/Headless%20macOS-333333?logo=apple&logoColor=white)](https://docs.wendy.dev/latest/installation/wendy-agent-macos/)
[![ESP32 Microcontrollers](https://img.shields.io/badge/ESP32-Microcontrollers-333333?logo=espressif&logoColor=white)](https://docs.wendy.dev/latest/installation/wendy-lite-esp32/)

Deploy code in less than a second from any machine to any Robot or Edge device.
- Supports any Linux Machine
- ESP32 Microcontrollers
- Headless macOS

You don't need any peripherals. Just your existing development machine and your target device.

### Any Developer Machine

![Ubuntu](https://img.shields.io/badge/Ubuntu-Supported-E95420?logo=ubuntu&logoColor=white)
![macOS](https://img.shields.io/badge/macOS-Supported-333333?logo=apple&logoColor=white)
![Windows](https://img.shields.io/badge/Windows-Supported-0078D6?logo=windows&logoColor=white)
![Arch Linux](https://img.shields.io/badge/Arch%20Linux-Supported-1793D1?logo=arch-linux&logoColor=white)

<p align="center">
  <img src="go/internal/cli/assets/docs/media/overhead-quick-install.gif" alt="Deploying an app to an NVIDIA Jetson with wendy run" width="640">
  <br>
  <em><code>wendy run</code> building and deploying an app to a Jetson over USB-C, with live logs.</em>
</p>

## Quick start

```sh
# 1. Install the Developer Tools
curl -fsSL https://install.wendy.dev/cli.sh | bash

# 2. Install WendyOS to your Robot, Jetson, or Raspberry Pi
wendy install

# 3. Build, deploy, and debug any (Docker) app
wendy run
```

Package-specific options are available via
[Homebrew, .deb, .rpm, and AUR](INSTALL.md).

### Common CLI Commands

```sh
wendy install # Install WendyOS on a device
wendy init # Create new project
wendy run # Build & Run an app
wendy run --watch # Watch for changes & Redeploy
wendy device top # Resource Stats
wendy device wifi # Manage wifi (connect/forget)
wendy device bluetooth # Game controllers, audio, etc..
wendy device ros2 # Interface with ROS2
```

## Starter Kits

<table>
  <tr>
    <td>
      <center>
        <img src="go/internal/cli/assets/docs/media/unitree-g1.jpg" alt="Unitree Teleop" width="150" />
        <img src="go/internal/cli/assets/docs/media/unitree-go2.jpg" alt="Unitree Teleop" width="150" />
        <img src="go/internal/cli/assets/docs/media/rosmaster.jpg" alt="Unitree Teleop" width="150" />
      </center>
    </td>
    <td>
      <h3>Remote Control & Teleop</h3>
      <ul>
        <li>📱 (Mobile) Browser Support</li>
        <li>🎮 Game Controller Support</li>
        <li>☁️ Works with <a href="https://cloud.wendy.sh">Wendy Cloud</a></li>
      </ul>
      <h3>Source Code</h3>
      Start through <code>wendy init</code> or clone from GitHub:<br />
      <a href="https://github.com/wendylabsinc/templates/tree/main/python/g1-rc">G1 Humanoid</a> | <a href="https://github.com/wendylabsinc/templates/tree/main/python/go2-rc">Go2 Quadruped</a> | <a href="https://github.com/wendylabsinc/templates/tree/main/python/rc-car">Rosmaster Car</a>
    </td>
  </tr>
  <tr>
    <td>
      <center>
        <img src="go/internal/cli/assets/docs/images/boards/ubuntu.png" alt="Unitree Teleop" width="300" />
        <br /><em>"Hey Jetson, turn off the lights upstairs."</em>
      </center>
    </td>
    <td>
      <h3>Sovereign AI</h3>
      Run local LLMs on your device in seconds.<br />
      Integrate with documents, video feeds, audio and more.
      <h3>Source Code</h3>
      Start through <code>wendy init</code> or clone from Github:<br />
      <a href="https://github.com/wendylabsinc/templates/tree/main/python/llm">Local LLM</a> | <a href="https://github.com/wendylabsinc/templates/tree/main/python/camera-feed-yolo">Object Detection</a> | <a href="https://github.com/wendylabsinc/templates/tree/main/python/voice-ai-pipecat">Voice Assisant</a>
    </td>
  </tr>
</table>

## Cloud

All `wendy` commands can be run across the globe with [Wendy Cloud](https://cloud.wendy.dev). Manage your fleet in seconds with OTA, remote debugging, health checks and more.

With `wendy mcp setup` - you can develop apps, manage fleet deployments and more through your favorite LLM provider.

For ChatGPT Desktop, run `wendy mcp setup chatgpt`, quit and reopen ChatGPT,
then install **Wendy** from **Plugins → Personal**. The local plugin includes
the MCP gateway, device workspace, and bundled skills. Follow the
[ChatGPT setup guide](docs/guides/chatgpt.mdx) to enable devices, simulators,
and project access.

```sh
wendy cloud run # Run apps from anywhere
wendy cloud device camera view # Remote video
wendy cloud device audio listen # Remote audio
wendy cloud ros2 bag download # Download ROS2 recordings
```

### Control your fleet from 30,000 feet. Literally.

No matter where you are, you're in control. Even on the beach.
