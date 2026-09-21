# Managed Go2 DDS isolation

Run the packet test against an existing local Linux image containing Python,
`nft` and `setpriv` (the Go2 build base or complete runtime image):

```sh
python3 simulator/go2/integration/isolation/exercise.py --image wendy-go2-build-base:dev
```

To prove the legacy fallback independently of whether the local kernel also
supports nftables, build the small test layer and select `--backend legacy`:

```sh
docker build -t wendy-go2-isolation-test:dev simulator/go2/integration/isolation
python3 simulator/go2/integration/isolation/exercise.py --image wendy-go2-isolation-test:dev --backend legacy --output simulator/go2/validation/legacy-isolation.json
python3 simulator/go2/integration/isolation/exercise.py --image wendy-go2-isolation-test:dev --backend nft --output simulator/go2/validation/legacy-isolation.json
```

The optional report keeps a separate result for each backend, including the
exact image ID and runtime source hashes. The legacy selector calls the actual
`go2_sim.isolation_legacy.install()` helper. The default nft selector invokes
`python3 -m go2_sim.isolation` with the runtime package on `PYTHONPATH`, allowing
its real fallback imports to work.

The harness creates two temporary containers on an **internal** Docker network
with IPv4 and IPv6. It exposes no host ports and never uses host networking,
a VM, or a physical robot. The guard has `CAP_NET_ADMIN` only to install and
inspect the runtime's actual firewall rules; legacy also needs `CAP_NET_RAW`
for libiptc's control socket. `CAP_SETPCAP` permits the subsequent privilege
drop test. The peer has no capabilities.
All resources carry a unique `wendy-go2-isolation-` prefix and are removed on
success, failure or interruption.

The JSON report records successful RTPS packets before installation, blocked
incoming and outgoing RTPS packets afterward, allowed loopback RTPS, allowed
ordinary UDP and HTTP in both directions, and the firewall's packet counters.
Each UDP case runs on ports 7417, 28901, and 55213. Actual receiver counts are
checked through HTTP; replies contain only a payload hash, so a blocked reply
cannot be mistaken for a blocked incoming packet. A prefixed RTPS marker
demonstrates the documented backend difference: nft matches the payload prefix,
while legacy KMP blocks the RTPS bytes anywhere in a non-loopback UDP packet.

The harness inserts broad ACCEPT rules before installation and verifies that
isolation still holds, while unrelated marker rules and policies remain intact.
It also checks repeated installation, preservation of a reserved table or
chain with a different owner, and that the runtime's `setpriv` flags remove
`CAP_NET_ADMIN` from every capability set and prevent further firewall writes
(both IPv4 and IPv6 writers for legacy).

This is a test of the managed UDP RTPS boundary. It does not establish a
security boundary against arbitrary privileged app code or encrypted and
tunneled transports. The guest runtime must pass its own startup installation
before reporting `dds_isolation: udp-rtps-loopback`. Forward-hook rule presence
is verified by installation/readback; this two-endpoint test does not exercise
a routed third network hop.
