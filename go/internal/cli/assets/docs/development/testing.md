# Testing

WendyOS uses both unit tests and integration tests to validate the CLI and agent behaviour.

## Routing engine tests

The routing library in `go/babel` is a separate Go module. A parent-module
`go test ./...` or `go vet ./...` does not include it. Run both checks from its
own directory:

```sh
cd go/babel
go test -race -count=1 -timeout=300s ./...
go vet ./...
```

The CI aggregate `Test` check requires the dedicated Babel job. The library's
[README](https://github.com/wendylabsinc/WendyOS/blob/main/go/babel/README.md)
contains additional fuzz and benchmark commands.

## Integration Tests

Integration tests verify that specific features work end-to-end on a real (or simulated) Wendy device. They live under `.github/ci-tests/` in the repository.

### Directory structure

Each integration test is a self-contained directory:

```
.github/ci-tests/<name>/
```

The naming convention is `<language>-<feature>`, for example `python-camera` or
`swift-filesystem`. Entitlement boundaries use paired positive and negative
fixtures where useful: `python-notifications` verifies the private app
connection is present, while `python-no-notifications` verifies it is absent
without the entitlement.

Depending on the language, a test directory contains:

**Python tests:**
```
.github/ci-tests/<name>/
  wendy.json          # App identity and entitlements
  main.py             # Test application
  Dockerfile          # FROM python:3.11-slim
```

**Swift tests:**
```
.github/ci-tests/<name>/
  wendy.json          # App identity and entitlements
  Package.swift
  Sources/<Target>/main.swift
```

**Compose tests:**
```
.github/ci-tests/<name>/
  docker-compose.yml  # No wendy.json needed
```

### `wendy.json` format for CI tests

```json
{
  "appId": "sh.wendy.ci.<name>",
  "platform": "linux",
  "version": "1.0.0",
  "language": "python",
  "entitlements": [...]
}
```

### Test behaviour

- Test apps must print a line containing `PASS` and exit `0` on success.
- Negative/blocking tests (verifying a capability is absent) should exit `1` if the blocked capability accidentally succeeds.
- Each test is single-purpose: it proves exactly one feature works.

Both signals are required. An attached `wendy run` exits `0` whatever the container did, so the harness reads the exit code back from `wendy device apps list --json` instead — and falls back to the printed verdict on agents that record no exit code, or when an app deployed under a `serviceName` reports only a running state. A test that prints no verdict can only be judged by the exit-code path.

Assert on something the feature actually changes inside the container. A signal that is present whether or not the entitlement was granted proves nothing: `/sys` is a plain read-only sysfs mount, so device nodes under `/sys/class/*` are visible to every container regardless of entitlements. Write the negative test alongside the positive one and check that each fails when the entitlement is flipped — a test that cannot fail is worse than no test.

### Running integration tests locally

Tests are driven by `go/scripts/test-ci.sh`. Its `ALL_TESTS` array is the single source of truth for what runs: adding a name there is all that is needed, and CI runs whatever it lists.

```sh
bash go/scripts/test-ci.sh <name>
```

Refer to the `usage()` block in that script for a full list of available test names and options.

### Identifying the device under test

Before running anything, the harness logs the agent version, OS, device type and architecture it found via `wendy device info`:

```
==> Agent version: 2026.07.27-003050
==> Device OS: wendyos WendyOS-0.17.0
==> Device type: jetson-agx-thor (arm64)
```

Read this first when triaging a failure. A result is only meaningful against a known agent, and an out-of-date agent produces failures indistinguishable from product bugs. Two values are worth recognising:

- `Agent version: dev` — an unstamped binary, so a hand-built agent rather than a release. Push a current one with `wendy device update --device <host>`.
- `Device type: none reported` — no `/etc/wendyos/device-type`, so the target is a generic Linux agent install and not a WendyOS image. Such a host is skipped by the nightly OS update and has no auto-updater, so its agent only changes when someone deploys one.

### CI execution

Integration tests run in `.github/workflows/integration-tests.yml` on both macOS and Linux runners. Neither job names the tests it runs — the harness runs `ALL_TESTS` and decides what is applicable, so a newly registered test needs no workflow change.

The harness skips a test rather than failing it when the host or device cannot support it:

| Tests | Skipped unless |
| --- | --- |
| `swift-*` | the build host has a `swiftly` toolchain (checked instead of `swift`, which on macOS is an Xcode shim that exists with no usable toolchain behind it) |
| `*-gpu` | the device reports `gpuVendor: nvidia` and a supported architecture. Orin (`sm_87`) uses the JetPack-6 / CUDA-12 fixtures; Thor (`sm_110`) and Spark (`sm_121`) use Ubuntu-24.04 / CUDA-13 fixtures. An older agent with no architecture uses CUDA 13 when its device type is `jetson-agx-thor` and otherwise retains the CUDA-12 path for compatibility. Other reported architectures are skipped until a matching fixture is hardware-verified. |

#### Stable release gate

When a release build is triggered with `publish=true`, the full macOS integration test suite runs as a required gate before the `release` and `publish-linux-repos` jobs proceed. If integration tests fail, neither job runs. Nightly builds (triggered by a `push` event or `publish=false`) skip the integration-test gate and proceed directly to release.

#### PR gate for CI config changes

Pull requests from within the repository that modify files under `.github/` automatically trigger a macOS integration test run via `.github/workflows/pr-integration-tests.yml`. Only one run is active per PR at a time; opening a new commit cancels the previous run.

---

## Automated Integration Test Coverage (CI)

`.github/workflows/integration-test-review.yml` checks whether a PR introduces CLI features, device capabilities, entitlements, or deployment modes that no integration test covers, and suggests the missing tests.

### How it works

1. **Trigger:** `pull_request` opened, synchronize, or reopened against `main`, for branches in this repository only.
2. **Diff fetch:** Downloads the PR diff via the GitHub CLI.
3. **Analysis:** Sends the diff, the PR title/body, the existing CI tests, `go/scripts/test-ci.sh`, and `.github/workflows/integration-tests.yml` to Claude, with a prompt describing the test layout and the registration step. It answers `NO_CHANGES_NEEDED` when nothing is missing.
4. **Suggestion:** Posts the reply as a PR review comment containing the full file content for each suggested test. Nothing is committed — apply, adapt, or dismiss the suggestions yourself.

The suggestions are only as current as that prompt. When the harness changes how tests are registered, run, or skipped, update the prompt in the workflow too, or it will keep advising authors to edit things that no longer exist.
