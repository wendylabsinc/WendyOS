The operator-router hello and ready envelopes accept optional `w`, an unsigned
maximum management window in bytes. Absent or zero means the legacy 32 KiB
limit. Nonzero values must be between 32 KiB and 256 KiB. Both peers must opt in;
the server selects the smaller maximum. The client rejects an unoffered value.
The current implementation offers 256 KiB only after authenticating the peer
and observing a direct `*net.TCPConn` beneath TLS. BLE, relay streams and unknown
wrappers retain 32 KiB. Stream open/ready credits cannot exceed the selection;
subsequent credits cannot exceed the receiver's initial grant.

`lite_agent_window.c` checks the existing Wendy Lite production CBOR parser with
an independent legacy hello, the same hello extended by `w = 262144`, every
truncation of the extended hello, and Lite's unchanged ready response. It was
validated against Wendy Lite revision `e7148ffc8b`. From the WendyOS repository
root, set `lite_components` to the sibling Lite components directory. The linker
must discard unused sections, because `control.c` also references PSA crypto
symbols the fixture never calls; macOS and Linux spell that flag differently. On
Linux without an esp-pqc checkout, the system Mbed TLS PSA headers also work:

```sh
lite_components=../wendy-lite/components
case "$(uname)" in Darwin) gc=-Wl,-dead_strip ;; *) gc=-Wl,--gc-sections ;; esac
clang -std=c11 -Wall -Wextra -Werror -ffunction-sections -fdata-sections "$gc" \
  -I "$lite_components/wendy_net_peer/include" \
  -I "$lite_components/wendy_net_gossip/include" \
  -I "$lite_components/wendy_net_common/include" \
  -I "$lite_components/wendy_pki/include" \
  -I "$lite_components/esp-pqc/components/mbedtls/mbedtls/tf-psa-crypto/include" \
  -I "$lite_components/esp-pqc/components/mbedtls/mbedtls/include" \
  -I "$lite_components/esp-pqc/components/mbedtls/mbedtls/tf-psa-crypto/drivers/builtin/include" \
  go/internal/shared/meshoperatorlink/testdata/lite_agent_window.c \
  "$lite_components/wendy_net_peer/src/control.c" \
  "$lite_components/wendy_net_peer/src/binary.c" \
  "$lite_components/wendy_net_common/src/binary.c" -o /tmp/lite-agent-window
/tmp/lite-agent-window
```

The Go compatibility tests run without a Lite checkout. The throughput benchmark
uses authenticated TLS, actual management streams and a bounded TCP proxy with
5 ms propagation delay in each direction. It compares a legacy handshake against
a negotiated one; proxy allocations are included, so allocation counts describe
the whole fixture, not just the management stream.

```sh
go test -race ./go/internal/shared/meshoperatorlink
go test ./go/internal/shared/meshoperatorlink -run '^$' -bench '^BenchmarkAgentWindowTCP$' -benchtime=5x -count=3
```
