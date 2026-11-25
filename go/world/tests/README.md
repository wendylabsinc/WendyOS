# World verification tests

Run deterministic Python checks from `go/world`:

```sh
python3 -m unittest discover -s tests -p 'test_*.py'
```

These tests use temporary files, synthetic protocol fixtures, mocked launchers,
and bounded loopback HTTP. They do not deploy to devices. The request trace
analysis implementation under `docs/verification` is preserved source code, not
a runtime receipt. The three retained historical JSON fixtures there, including the compressed
first-fault capture, are required by geographic and Go2 executor regression
tests. Their hashes and assertions preserve original observations; they are not
fresh acceptance evidence.

Original recorded Go2 checks require an external archive. Set
`WENDY_WORLD_VERIFICATION_ARCHIVE` to the absolute directory corresponding to the
original World checkout's `docs/verification`, then run the same command. Keep
that archive outside this repository. The required paths are:

- `go2-cloud/capture/{a,b}/passive-{a,b}.json.gz` and matching
  `passive-{a,b}-runtime-before.json` / `passive-{a,b}-runtime-after.json`.
- `go2-zero-selection/preparation/candidate-profile.json` and
  `candidate-alignment.json`.
- `go2-cloud-live/cleanup/status.json` and `scene-state.json`.
- `go2-cloud-shadow/preparation/initial-scene.json.gz`.
- `go2-cloud-live/trials/r/observer-samples.ndjson.gz`.

Without the environment variable, the two recorded-cloud test classes and two
archive-specific preflight tests report an explicit skip. Synthetic preflight
validation, malformed-input, timeout, and response-bound checks still run. An
explicit but invalid or incomplete archive fails; it never silently substitutes
synthetic data. Recorded results remain historical simulation evidence.

The Linux verification helpers resolve Go through PATH and the WendyOS root
module. Their source manifests include root `go.mod`/`go.sum`, the local Babel
module declaration, shared SensorLink protobuf definitions, and World source.
Use their explicit image/output options for heavier isolated Linux checks.

Migration verification on November 25, 2026 used the existing Linux image with
MuJoCo 4 and Pillow 13, networking disabled and source mounted read-only:
139 tests without the archive, with four skips; 154 tests with the original
archive mounted read-only, with no skips. This verifies tests and recorded-data
replay, not a new hardware run or native UI acceptance.
