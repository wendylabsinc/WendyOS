# Wendy Lite Repository

The wendy-lite source code lives in [wendylabsinc/wendy-lite](https://github.com/wendylabsinc/wendy-lite) — the Swift SDK, Rust crate, C headers, and ESP-IDF firmware component all live in this single repo.

## Repository Layout

```
wendy-lite/
  Sources/
    CWendyLite/
      include/wendy.h        Host-function declarations (WASM import attributes)
      shim.c                 Thin C shim for Swift interop
    WendyLite/               Swift SDK (SwiftPM library target)
      WendyLite.swift        Re-exports CWendyLite
      WendyLiteApp.swift     @main protocol + async runtime bootstrap
      WendyClock.swift       Embedded-Swift async clock + TimerHub
      CallbackDispatch.swift Handler registry + wendy_handle_callback export
      GPIO.swift             GPIO types and wrapper enum
      I2C.swift              I2C wrapper
      SPI.swift              SPI wrapper
      UART.swift             UART wrapper
      RMT.swift              RMT (remote control) wrapper
      NeoPixel.swift         NeoPixel/WS2812 wrapper
      Timer.swift            Low-level timer wrapper
      Network.swift          WiFi, Net (sockets), DNS, TLS wrappers
      BLE.swift              BLE, GATTS, GATTC wrappers
      OTel.swift             OpenTelemetry log/metrics/tracing wrapper
      Storage.swift          NVS key-value wrapper
      System.swift           System + Console wrappers
      USB.swift              USB CDC + HID wrapper
  src/lib.rs                 Rust crate (no_std FFI + safe wrappers)
  CMakeLists.txt             ESP-IDF component CMake (firmware build)
  Package.swift              SwiftPM package (Swift SDK)
  Cargo.toml                 Cargo package (Rust crate)
  .github/workflows/
    build.yml                Matrix firmware build + nightly/release publishing
```

## CI and Releases

Wendy publishes installable firmware variants through its stable and nightly release channels. `wendy install` reads the current firmware catalog and only presents board variants that have a published binary for the selected channel.
