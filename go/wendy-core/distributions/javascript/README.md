# Wendy Core for JavaScript

One ESM package and one version, with Node/native and browser/Go-WASM backends.

```js
import { WendyHelloWorld } from '@wendylabs/core';
console.log(WendyHelloWorld()); // 42
```

Shared TypeScript declarations and generated operation forwarding mirror the C contract. This is the Core incarnation, not an ergonomic Wendy JavaScript SDK. Conditional exports choose the backend; do not deep-import private native shims. Modern browsers need ESM, top-level await, WebAssembly and Web Crypto. The browser backend performs asynchronous initialization at module import; the operation itself is synchronous in both runtimes.

Browser hosts/bundlers must serve the packaged `assets/wasm` files at the URLs relative to the browser entry. Node consumers need a supported glibc/macOS/Windows target, but no Go compiler or native-build toolchain: the released package contains the binaries. Other runtimes are not claimed yet.

The bundled Go runtime and compiled Go engine carry the Go project's BSD notice under `assets/**/LICENSE-Go.txt`.
