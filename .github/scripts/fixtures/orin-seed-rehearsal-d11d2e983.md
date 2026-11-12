# Full Orin host-writer seed rehearsal

This gate exercises the existing `TestOrinFullInstallSeedSim` against all bytes
of a real T234 flashpack. It retains the 128 GiB sparse target and three actual
`t234.RunWriter` passes. Runs two and three first convert the preceding config
partition to ext4. Every pass reads the injected and flashed FAT images, parses
the requested name/Wi-Fi values using the Agent's seed semantics, checks the
explicit arm64 Agent bytes, rejects stale ext4 metadata, and checks that the
cached config image stayed unchanged.

The second binary omits only the production `wendy.conf` write through a Go
overlay. It must finish a full writer pass and then fail flashed-seed readback.
A compile error, timeout, skipped prerequisite, or injection-only failure is
not an accepted control result. No production source is edited.

This is host-side regular-file coverage for `d11d2e983`, not a USB/RCM transfer,
physical first boot, Wi-Fi connection, OS mount/eject, or application of the
seed by a running board. The physical first-boot requirement in `49fa7f963`
remains separate. The original close/unmount error tests remain relevant.

## Existing producer integration

Use the existing `wendylabsinc/WendyOS-Builder` Linux x64 `wendyos-builder`
job, after **Build flashpack (Jetson)** and before **Upload artifacts**. The
job already creates the entire archive in
`$GITHUB_WORKSPACE/flashpack-out/${MACHINE}-${STORAGE}`; it does not need to
download another multi-GB copy or allocate another runner. Run the Nano/NVMe
case in the existing matching matrix row. Keep all three passes.

Prerequisites are a separate checkout of this CLI repository at the reviewed
commit, Go matching its root `go.mod` (currently 1.27.4), Python 3.9+, `zstd`,
and an ordinary writable Linux filesystem supporting sparse files and hard
links. Do not reuse or clean the producer's `/opt/wendy-cache` or build tree.
The driver makes and removes only a fresh `wendy-orin-seed-*` work directory.
It never uses sudo, Docker, block devices, host mounts or a shared builder.

The descriptor must bind the exact archive produced by this job: device,
storage, release version, intended public archive URL, full SHA256 and byte
length. Build it from the producer's known release variables and the finished
archive, retaining it with evidence. `orin-seed-0.20.2.json` is an alternative
fixed released fixture, with a GCS generation and full hash; it is usable only
when those exact bytes are already available locally. Do not silently use that
descriptor for another release. The archive's own version is checked before
extraction. Production flashpack validation remains responsible for its full
manifest and consumed-file hashes.

From the separate CLI checkout, with `archive` and `descriptor` identifying
those existing files, execute:

```sh
python3 .github/scripts/test-orin-seed-rehearsal.py \
  --archive "$archive" --fixture "$descriptor" \
  --work-parent "$RUNNER_TEMP" \
  --output "$RUNNER_TEMP/orin-seed-evidence"
```

The output directory must be new. An existing real arm64 Agent can be reused
with `--agent-binary /absolute/path/to/wendy-agent`; otherwise the driver builds
one with `CGO_ENABLED=0 GOOS=linux GOARCH=arm64`. Go compile/cache permissions
remain those of the ordinary producer user.

Allow at least 40 GiB free **in addition to the existing producer archive**.
Before extraction/builds the gate hashes the entire archive and streams its
tar headers. It then requires the greater of 40 GiB or twice all regular-file
logical sizes plus eight times the larger of config-image/config-partition
size (from the real XML layout) and 4 GiB reserve. That accounts
for extracted payloads, dense writer output, retained config readbacks,
mutable config and GPT/zeroing/metadata/binaries. A different-filesystem
archive additionally reserves its full compressed size for the private copy.
The 128 GiB target is sparse; evidence records both apparent and allocated
bytes. An insufficient filesystem fails before extraction; no payload or
iteration is removed to fit it.

Bounds are 15 minutes for streamed preflight, 10 minutes each for the Agent
and two test binaries, 30 minutes for three normal writer passes, and 15
minutes for the control. Reserve 100 minutes in the existing producer job
after its image build. These are failure deadlines, not measured runtimes;
the first real producer execution must establish duration and capacity.
If that job cannot supply these resources, this gap stays pending until an
existing suitable job is wired. No new runner is required by this change.

The pinned process-group guardian drains owned subprocesses on timeout or
handled interruption. Normal and control disk targets never coexist. Cleanup
removes only the driver's private workspace, including its private flashpack
cache; the producer archive remains unchanged. Logs, descriptor, streamed
allocation report, binary/source hashes, source commit, durations and result
JSON remain in the requested output directory. Retain those small files on
the existing CI job; do not upload or commit the sparse disk/config images.

## Local validation scope

Portable Python tests exercise real tar header accounting and reject false
normal/control evidence. Go helper tests hash and stage an actual private
file, reject a bad digest/existing destination, and verify that removing the
private archive link leaves the producer bytes intact. These checks do not
replace the full producer run, which remains pending until hosted evidence
contains all three verification markers and the rejected control.
