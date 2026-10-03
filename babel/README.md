# Babel, without sockets

An experimental, standard-library-only Go routing engine for explicitly configured
point-to-point links. Implements a destination-routing profile of RFC 8966 and the
RFC 9229 IPv4-via-IPv6 extension. It is not an IP stack, QUIC implementation, service
directory, network manager, or production-certified routing daemon.

This nested module is intentionally independent of Wendy's agent module. Nothing
in the existing NAN/BATMAN backend imports it yet.

```go
engine, err := babel.New(babel.Config{RouterID: 1, IPv4ViaIPv6: true})
// Handle err. Provision one Link per authenticated, scoped peer session.
effects, err := engine.Step(elapsed, babel.AddLink{Link: babel.Link{
    ID: 1, Local: localLinkLocal, Peer: peerLinkLocal, Cost: 96,
}})
// Handle err. For every returned effects batch:
if effects.Revision != 0 {
    // Atomically apply the complete Routes snapshot, including Unreachable drops.
    effects, err = engine.Commit(effects.Revision, forwardingSucceeded)
}
// Persist engine.Checkpoint() before transmitting effects.Datagrams if crash-safe
// restart is required. Then send datagrams on their scoped links.
// Use NextDeadline to schedule Step(elapsed, babel.Tick{}).
```

Use `Receive` for complete Babel payloads on an already validated link. Link-local
addresses can repeat on different links; next hops must always retain the LinkID.
Do not run concurrent engine calls. There are no hidden goroutines or clocks.
Snapshots and effects are caller-owned. `Commit(false)` is terminal and requires
the embedding application to stop forwarding. See [profile](docs/profile.md) for
the complete contract and [conformance](docs/conformance.md) for evidence/limits.

`mobile` offers a serialized JSON/byte-array facade suitable for language bindings;
`cmd/babelabi` is an experimental C ABI build target. These are not stable ABIs.
The mobile facade does not itself enable iOS background networking or VPN privileges.

## Development

Run from this directory, not the parent module:

```sh
go test -race ./...
go vet ./...
go test ./internal/wire -run '^$' -fuzz FuzzDecode -fuzztime 30s
go test ./internal/sim -run '^$' -fuzz FuzzChurn -fuzztime 30s
go test ./internal/sim -run '^$' -bench . -benchtime 1x -benchmem
```

The integration test commands in [oracle instructions](docs/oracle.md) run native
Linux network namespaces with real babeld peers. Do not run the test daemon on a
normal host: it installs kernel routes. No production integration is included.
