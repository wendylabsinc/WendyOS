# Wendy Lite

Wendy Lite is Wendy's runtime and deployment layer for ESP32 microcontrollers. It supports native ESP-IDF applications as the recommended project model and can also run portable WASM guest applications.

## Role in the Wendy Platform

The broader Wendy platform targets Linux/macOS edge devices (Raspberry Pi, Jetson, Mac) via WendyOS and wendy-agent. Wendy Lite covers bare-metal MCUs where containers and a full OS are not viable. Native apps use the complete ESP-IDF API and normal ESP-IDF project layout; `wendy run` detects, builds, and deploys them. WASM remains available when a smaller portable application boundary is preferable.

> **Recommendation:** Start new applications as regular native ESP-IDF projects. Use the optional WASM runtime when portability or sandboxing matters more than full ESP-IDF access. Camera and display/framebuffer peripherals are not exposed to WASM guests and should be driven by native ESP-IDF drivers.

## Supported Targets and Boards

Wendy Lite supports the following ESP32 targets:

- ESP32-C5
- ESP32-C6
- ESP32-C61
- ESP32-P4
- ESP32-S3

For each target, `wendy install` lists the specific boards Wendy Lite supports. Choosing a board selects a Wendy Lite firmware variant pre-configured with that board's flash size, RAM, and peripherals. However, most targets also have a **generic** board, representing any board that exposes the SoC over USB with no further customization. If your specific board isn't listed, you can probably use the generic one instead.

Native app capability is firmware-specific; where the installer offers multiple choices, select a board variant labeled **native app support**. Native applications are built with ESP-IDF 5.5.4 through the ESP-IDF Installation Manager (`eim`).

## Documentation

### User Guides
- [Getting Started](getting-started.md)
- [Native Apps](native-apps.md)
- [WASM Apps](wasm-apps.md)
  - [Host API Reference](host-api.md)

### Implementation Details
- [Source Code](repository.md)
- [Swift SDK Internals](swift-sdk.md)
- [StdIO](stdio.md)
- [WendyCom Protocol](wendy-com.md)
