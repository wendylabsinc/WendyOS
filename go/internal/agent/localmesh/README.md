# Local mesh routing core

This is the staged Linux local-mesh router from the NAN prototype, rebased on
current WendyOS. It uses enrolled Wendy device certificates, mTLS QUIC links,
signed device/gateway manifests, a Babel route engine, and Wendy-owned TUNs and
routes. `local-mesh.json` activates an explicitly configured TCP topology. A
missing file leaves this new router disabled.

```json
{
  "listen": "0.0.0.0:43020",
  "peers": [
    {"asset": 460, "address": "192.0.2.60:43020"}
  ]
}
```

Place the file in the agent config directory (`/etc/wendy-agent` by default)
on **both endpoints**. The lower asset ID initiates each edge; the higher ID
listens. An incoming asset must be in the peer list. The TCP carrier preserves
packet boundaries for QUIC and pins the expected org and asset certificate.
It exists to simulate chosen graph topologies with ordinary TCP listeners and
clients. TCP's head-of-line blocking makes it inappropriate as the final LAN,
NAN or Bluetooth carrier. Use it only in isolated Linux test environments.

The current module routes host `10.88.0.0/16` addresses and can carry IP over
several Babel hops. It does **not** yet connect the existing app-facing
`10.99.0.0/16` VIP proxy to this route. It also does not implement app service
publication, mDNS, or a general end-to-end app QUIC session. The existing
WendyOS mesh-mode app path still selects direct LAN or cloud relay. These are
the next local-mesh integration gates, not implied by this branch.

Production review is still needed for named-mesh admission, addresses beyond
16-bit asset IDs, route ownership, transport failure recovery, resource limits
and host cleanup after an abrupt process exit. The signed manifest authenticates
its origin, but a relayed Babel reachability claim is not itself signed.

Run from the WendyOS root:

```sh
go test -race ./go/internal/agent/localmesh
go vet ./go/internal/agent/localmesh
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go test -c ./go/internal/agent/localmesh -o /tmp/wendy-localmesh-arm64.test
```

Linux tests with `WENDY_LOCALMESH_ISOLATED_TEST=1` mutate network interfaces,
routes, forwarding and DNS. Run them only inside a disposable privileged
container or namespace, never in a host or Jetson namespace.
