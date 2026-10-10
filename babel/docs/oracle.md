# Reproducing validation

## Local unit, simulation and fuzz tests

Run `go test -race ./...` and `go vet ./...` in the module directory. Use
`go test -coverpkg=.,./internal/wire ./...` to include engine coverage exercised by
the external simulator; ordinary per-package coverage misses those calls.

Use the commands in README for bounded fuzzing and scale benchmarks. Go's fuzzer
minimizes failing byte sequences automatically; its deterministic event sequence
then reproduces the stateful simulation. Preserve failures as testdata, not merely
logs. Benchmark allocation totals are cumulative, not live memory measurements.

## Native Linux mixed-daemon tests on Mac

From the workspace root, with the local reference already at the pinned commit:

```sh
git -C nan/babeld submodule update --init --recursive
docker run --rm \
  -v "$PWD/nan/babeld:/source:ro" -v wendy-babel-oracle:/build \
  golang:1.26.6-bookworm sh -c \
  'cp -a /source/. /build/ && cd /build && make -j4 && ./babeld -V'
docker build -t wendy-babel-oracle:20260911 \
  -f WendyOS/babel/internal/oracle/Dockerfile WendyOS/babel/internal/oracle
mkdir -p nan/babel-artifacts/interop
docker run --rm --network none --privileged \
  -v "$PWD/WendyOS/babel:/module:ro" \
  -v wendy-babel-oracle:/reference:ro \
  -v "$PWD/nan/babel-artifacts/interop:/artifacts" \
  -v wendy-babel-gocache:/root/.cache/go-build \
  wendy-babel-oracle:20260911 bash internal/oracle/run.sh
```

Use a new artifact folder and `internal/oracle/transit.sh` for the reverse roles:
babeld → Go → babeld, including an unannounced bidirectional packet-loss interval.
Use separate disposable containers for scenarios; the scripts intentionally do not
reuse namespaces. Docker's Linux VM runs ARM64 natively here; do not add amd64 platform
emulation. `--privileged` is used only to enable isolated network namespaces and
their mount setup. The container has no external network, read-only sources and no
host Docker socket. Never run these namespace/route scripts on the host directly.

Reference: babeld `118774d0c720cef016c0f3894ef8d24cd9cadd17` (reports 1.14), BLAKE2
`320c325437539ae91091ce62efec1913cd8093c2`, rfc6234
`285c8b86c0c6b8e9ffe1c420c5b09fa229629a30`. No reference routing-code patches.
Build flags are the upstream defaults (`-Os -g -Wall`), GCC from the Docker image.
Do not reuse a volume built with different test/compiler flags without rebuilding
its objects. Record `git rev-parse HEAD`, submodule status, compiler and image digest
with new results. The test image uses Debian package repositories, not an immutable
package snapshot; captures should always identify the actual environment.

The scripts save pcap, protocol snapshots, daemon logs and kernel routes. Compare
reachable prefixes, origin and acceptable metrics; do not compare jitter or
equal-cost choices byte-for-byte. `run.sh` records babeld's management dump over
IPv6 loopback (`::1`), not 127.0.0.1.

## C/mobile boundary

The mobile package deliberately uses only strings, integers and byte arrays in its
method API. JSON checkpoints must be treated as opaque bytes (router IDs can exceed
JavaScript's exact integer range). Objects serialize calls with a mutex, but callers
must still honor the Step/Commit protocol. C handles must be valid and destroyed
exactly once; destruction must not race any other call.

Native Mac smoke test, run from this module:

```sh
go build -buildmode=c-archive -o /tmp/babel.a ./cmd/babelabi
clang -I /tmp mobile/smoke.c /tmp/babel.a \
  -framework CoreFoundation -framework Security -lresolv -o /tmp/babel-smoke
/tmp/babel-smoke
```

For iOS, use `GOOS=ios GOARCH=arm64 CGO_ENABLED=1`, Xcode's clang with the iPhoneOS
SDK sysroot and `-target arm64-apple-ios15.0`, and build `cmd/babelabi` as a C archive.
The project worklog records the exact SDK used for the verified build. This produces
a library, not a signed app or an automatically configured Network Extension.

Android's pure-Go packages can be checked with `GOOS=android GOARCH=arm64 CGO_ENABLED=0
go build . ./mobile`. A real `c-shared`/JNI artifact additionally requires a complete
Android NDK; do not equate pure-Go cross-compilation with a tested Android app.
