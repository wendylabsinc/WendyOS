# Wendy authenticated-time extraction

This private extraction reads the exact public Wendy Git revision identified in
`TIME-SOURCES.json`. It never copies uncommitted public working-tree changes.
The manifest records original Git blobs and source SHA-256 values, plus hashes
of adapted and private files. The public root Apache-2.0 license is retained in
`WENDY-LICENSE`; preserve it and this change notice on distribution.

The extracted Roughtime client and message codec depend only on Go's standard
library. Native boot-clock and boot-identity calls use the module's existing
`golang.org/x/sys` and `github.com/google/uuid` dependencies, whose notices are
already retained in `X-SYS-LICENSE` and `UUID-LICENSE`. This increment adds no
module dependency, CA, private key format or credential-storage mechanism.

## Extraction and intentional differences

- `internal/upstreamtime/roughtime/{client,message,servers}.go` retain Wendy's
  Roughtime framing, Ed25519 context strings, delegation verification, nonce
  Merkle proof, supported versions, ten-second radius bound and pinned public
  server list. The transport uses contextual dialing and cancellation, rejects
  oversized UDP datagrams, and validates key length before signature verification.
  Integer timestamp conversions reject overflow. Delegation validity is enforced
  whenever CERT is present, including a zero MAXT; upstream used zero as an
  absence sentinel and could accept a fresh response under a delegation valid
  only at the Unix epoch. A signed local-server regression reproduces that
  failure and verifies refusal. The codec additionally refuses
  duplicate or unsorted tags. These stricter input checks do not replace any
  upstream signature or freshness check.
- Roughtime client and Merkle tests retain their behavior with a private import
  path and provenance comment. They use only local generated signing keys.
- `internal/upstreamtime/consensus.go` retains the upstream largest-intersection
  selection and two/three-source confidence thresholds. The private wrapper
  supplies a checked native boot clock instead of using a package-global clock.
  Per-response time/radius bounds are checked before offset arithmetic. The
  untrusted wall-clock diagnostic timestamp was removed from acquisition; its
  legacy field remains zero. The five-second response RTT bound is unchanged.
  Existing intersection tests are retained with a package rename.
- `authenticatedtime` adapts `Manager.FreshTime` policy without copying the
  manager or its clock-discipline facilities: fresh nonce-bound quorum, no
  clock-floor read/write, acceptance of a completed quorum when an unavailable
  peer reaches the collection deadline, and the same 2024–2100 plausibility
  range. It requires explicitly supplied source pins and finite budgets,
  rejects repeated keys/addresses/names, and fails on caller cancellation.
- The private adapter widens for configured drift during collection and feeds
  bounded `trustedtime.Holdover`. A synchronized reader latches clock/boot
  faults during parallel queries and later holdover. Evidence reports are
  copies and cannot inject an authenticated anchor back into the runtime API.
- Linux keeps the upstream suspend-inclusive `CLOCK_BOOTTIME` choice and adds
  real boot UUID binding. The upstream non-Linux process-monotonic fallback is
  intentionally excluded. macOS uses `CLOCK_MONOTONIC_RAW` and
  `kern.bootsessionuuid`; unsupported systems return an error.

For macOS clock semantics, the locally installed macOS 27.0 `clock_gettime(3)`
manual distinguishes sleep-inclusive `CLOCK_MONOTONIC_RAW` from sleep-excluding
`CLOCK_UPTIME_RAW`. The installed SDK's `mach/mach_time.h` also documents
continuous time as advancing during sleep. The adapter uses the native syscall
through x/sys, without copying Apple implementation code or requiring cgo.

The original `fresh_time.go`, `floor.go`, `roughtime.go` and boot-clock files are
source references in the manifest. Only their stated policy and constants are
adapted: no system clock setter, floor persistence, multicast relay, enrollment
or agent manager enters the private runtime.
