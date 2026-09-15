# WASM Apps

Wendy Lite firmware variants with WASM support can run Swift, Rust, C/C++, AssemblyScript, or WAT guests as a smaller, portable, sandboxed alternative to native ESP-IDF apps. Guests call into hardware and platform features through Wendy's host-imported APIs — see [`host-api.md`](host-api.md) for the full function reference.

## Optional WASM Guest Languages

| Language | Entry point | Library |
|----------|-------------|---------|
| Swift | `@main` on `WendyLiteApp` | SwiftPM: `WendyLite` (this repo) |
| Rust | `#[no_mangle] pub extern "C" fn _start()` | Cargo: `wendy-lite` (this repo) |
| C / C++ | `void _start(void)` | Include `Sources/CWendyLite/include/wendy.h` |
| AssemblyScript | `export function _start()` | `@external("wendy", ...)` declarations |
| WAT | `(export "_start" ...)` | Direct import from `"wendy"` module |

## Building and Running WASM Apps

For Swift projects, `wendy run` builds the package for `wasm32-unknown-wasip1`, uploads the `.wasm` application, and attaches to its console. For other guest languages, or to embed a `.wasm` app directly into a firmware build, see [Manually Embedding a WASM App](#manually-embedding-a-wasm-app) below.

## Manually Embedding a WASM App

For low-level firmware development, a WASM guest can still be embedded directly:

1. Build your app to `.wasm`.
2. Convert the binary to a C header array: `./wasm_apps/wasm2header.sh my_app.wasm main/demo_wasm.h`
3. Rebuild the firmware: `idf.py build`
4. Flash: `idf.py flash`

See [`host-api.md`](host-api.md) for the full function reference, [`swift-sdk.md`](swift-sdk.md) for Swift-specific internals, [`stdio.md`](stdio.md) for console I/O, [`native-apps.md`](native-apps.md) for the native app build-to-device flow, and [`wendy-com.md`](wendy-com.md) for the WendyCom protocol reference.
