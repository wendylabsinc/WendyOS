# Browser WASM gRPC experiment

Runs Wendy's existing `grpcclient.ConnectWithTLSExpecting`, certificate verifier,
generated protobuf clients, and `clouddefaults.TunnelDialer` inside browser WASM.
The only new transport dependency is `github.com/coder/websocket`.

The browser carries Go TLS records in binary WebSocket messages. A local relay
unwraps those messages and copies the bytes to a TCP connection. TLS terminates
at the mock Wendy gRPC service. The relay does not terminate the inner TLS session.

## Run

From the repository root:

```sh
GOOS=js GOARCH=wasm CGO_ENABLED=0 go build -o /tmp/wendy-wasm-grpc.wasm ./go/experiments/wasmgrpc/client
go run ./go/experiments/wasmgrpc/server
```

Open `http://127.0.0.1:8787`. The page runs the checks and displays the result.
Use `-port 0` to select an available port or `-wasm PATH` to serve another build.
The server uses the running Go toolchain's matching `wasm_exec.js`.

For automated Chromium validation, with Playwright and its Chromium installed:

```sh
node go/experiments/wasmgrpc/browser.mjs http://127.0.0.1:8787 /path/to/playwright/index.mjs
```

Alternatively set `PLAYWRIGHT_MODULE` or make `playwright` resolvable by Node.

The checks cover:

- A real generated Wendy `GetAgentVersion` RPC. The server verifies the client
  certificate and checks the authenticated operator before returning a result.
- A `HostShell` bidirectional stream that echoes binary messages, including a
  512 KiB payload. Replies arrive before the client closes its send side.
- Reconnection on the same gRPC client after forcibly closing the WebSocket.
- Wendy's existing verifier rejecting a server in the wrong organization.
- The server rejecting a client that does not present a certificate.
- Browser-side confirmation that tunnel messages are binary.

## Scope

This is a transport proof of concept, not a port of the full CLI. The server
binds only to loopback, generates temporary ECDSA certificates, and exposes those
test credentials to the local page. It never loads real Wendy credentials or
opens a shell. The fixture uses a fixed TCP destination and same-origin checks.

The local outer connection uses `ws://`; the inner connection still uses TLS 1.3
with mutual authentication. A deployed relay needs HTTPS/WSS and authorization
for the requested device. Production credential storage, ML-DSA certificate
chains, persistent device pins, and native CLI commands are outside this test.
No production client or agent code is changed.
