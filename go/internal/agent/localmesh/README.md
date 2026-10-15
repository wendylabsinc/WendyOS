# Local mesh routing core

This is the staged Linux local-mesh router from the NAN prototype, rebased on
current WendyOS. It uses enrolled Wendy device certificates, mTLS QUIC links,
signed device/gateway manifests, a Babel route engine, and Wendy-owned TUNs and
routes. `local-mesh.json` activates configured TCP links, NAN and BLE
independently on one Babel node. A missing file leaves this router disabled.

```json
{
  "listen": "0.0.0.0:43020",
  "nan": true,
  "ble": true,
  "peers": [
    {"asset": 460, "address": "192.0.2.60:43020"}
  ]
}
```

Place the file in the agent config directory (`/etc/wendy-agent` by default)
on **both endpoints**. The lower asset ID initiates each edge; the higher ID
listens. An incoming asset must be in the peer list. The TCP carrier preserves
packet boundaries for QUIC and pins the expected org and asset certificate.
For a radio-only device, `{ "nan": true }`, `{ "ble": true }`, or both are
sufficient. NAN uses the BE202
`wendyos-nan` helper and an unencrypted NDP underlay; enrolled Wendy mTLS QUIC
protects mesh data and control traffic. The router prefers TCP cost 256 over
NAN cost 512 when both carry the same route. BLE advertises and scans at the
same time, listening on an LE L2CAP CoC PSM and dialing only lower asset IDs.
It uses pinned TLS 1.3 mTLS directly over that reliable channel, with Babel
cost 4096. The BLE advertisement carries asset ID, a hash of the default mesh
name, and PSM under a fixed org-scoped UUID; all hints remain untrusted until
the certificate handshake succeeds.
It exists to simulate chosen graph topologies with ordinary TCP listeners and
clients. TCP's head-of-line blocking makes it inappropriate as the final LAN,
NAN or Bluetooth carrier. Use it only in isolated Linux test environments.

The router carries host `10.88.0.0/16` addresses over several Babel hops.
When `local-mesh.json` is present, app-facing `10.99.0.0/16` VIP traffic uses
an end-to-end QUIC session on UDP 43021 to the peer's routed `10.88` address.
The peer certificate is pinned to the VIP's asset ID, and the destination
opens only host ports published by running isolated `mode: "mesh"` apps.
Each TCP flow uses a reliable QUIC stream. The opt-in proxy does not fall back
to the legacy cloud byte relay, which has no end-to-end app admission protocol.
App service publication, mDNS, and cloud app sessions remain separate gates.

Production review is still needed for named-mesh admission, addresses beyond
16-bit asset IDs, route ownership, transport failure recovery, resource limits
and host cleanup after an abrupt process exit. The signed manifest authenticates
its origin, but a relayed Babel reachability claim is not itself signed.

Run from the WendyOS root:

```sh
go test -race ./go/internal/agent/localmesh ./go/internal/agent/meshsession ./go/internal/agent/meshingress
go vet ./go/internal/agent/localmesh ./go/internal/agent/meshsession ./go/internal/agent/meshingress
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go test -c ./go/internal/agent/localmesh -o /tmp/wendy-localmesh-arm64.test
```

Linux tests with `WENDY_LOCALMESH_ISOLATED_TEST=1` mutate network interfaces,
routes, forwarding and DNS. Run them only inside a disposable privileged
container or namespace, never in a host or Jetson namespace.
