# HelloWorld examples

Each example calls `WendyHelloWorld()` in the shared Go engine and prints:

```text
WendyHelloWorld() = 42
```

Core dependencies come from this checkout on disk, not a published registry. Build the native resources before using a native distribution; each README includes the necessary commands. Toolchains and third-party build dependencies may still need downloading.

- [Go](go/README.md) — local Go module, without a C round trip
- [C](c/README.md) — local header and Go shared library
- [Swift](swift/README.md) — this repository's SwiftPM package with locally built static binaries
- [Python](python/README.md) — install the local Core package into a virtual environment
- [Kotlin/JVM](kotlin/README.md) — local Core jar
- [JavaScript](javascript/README.md) — local npm package, in Node and a browser

These are console examples (plus a browser page), not mobile apps. See the [bootstrap target matrix](../README.md#bootstrap-target-matrix) for current OS/architecture coverage. No publishing configuration is needed.
