# Swift HelloWorld

Requires Go, Python 3.9+, and Swift 6.2+; on macOS also Xcode. Current targets are macOS 15+ (arm64/x64) and Linux x64/arm64. Wendy Core also supports iOS 18+, but this console example does not run there.

From the repository root, build the static Swift binaries for this host:

```sh
python3 scripts/build.py swift
```

On macOS, install XcodeBuildMCP if needed (`npm install -g xcodebuildmcp@2.7.0`), then run:

```sh
xcodebuildmcp swift-package run --package-path "$PWD/examples/swift/hello-world" --executable-name HelloWorld
```

On Linux:

```sh
cd examples/swift/hello-world
swift run HelloWorld
```

Expected output: `WendyHelloWorld() = 42`.

`hello-world/Package.swift` depends on the repository root on disk. Its `Package.swift` uses the binaries just built; no release tag is needed.
