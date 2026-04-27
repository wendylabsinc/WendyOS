# kb.mac-integration-tests plan

## Goal

Clean up the naming around our real-device test lane so it reflects what these
artifacts actually are and leaves room for the growing CLI/device/platform
matrix.

This branch is about naming and terminology only. It is not changing test
coverage, fixture behavior, or the underlying deployment paths.

## Terminology to use going forward

For this work, use the following terms consistently:

- **CLI**: the side where `wendy` runs
- **device**: the side where the agent runs
- **platform**: the app execution platform on the device, currently `linux` or
  `darwin`

Avoid for now:

- `controller`
- `machine`
- `runtime`
- a formal `target` abstraction

We do not need to define `target` yet. `CLI`, `device`, and `platform` are
sufficient for the current cleanup.

## Why this cleanup is needed

The old names:

- `go/scripts/test-ci.sh`
- `.github/ci-tests/`

had become misleading.

They describe where the files happened to be used historically, not what they
actually represent now. These are no longer just generic “CI tests”; they are
our shared real-device test harness and fixtures.

The current matrix also makes older wording like `controller` increasingly
awkward:

- multiple CLI platforms: Linux, macOS, Windows
- multiple devices: RPi5, Jetson, Mac
- multiple device platforms now or soon: `linux`, `darwin`

## Current state after the first rename

Already completed in this branch:

- `go/scripts/test-ci.sh` -> `go/scripts/run-integration-tests.sh`
- `.github/ci-tests/` -> `.github/integration-tests/`
- references updated in `.github/workflows/integration-tests.yml`

Intentionally unchanged for now:

- `.github/workflows/integration-tests.yml`

This keeps the first step small and low-risk.

## Proposed end state

We should move from **integration** naming to **E2E tests** naming for this
lane.

### Desired paths

- `.github/workflows/e2e-tests.yml`
- `go/scripts/run-e2e-tests.sh`
- `.github/e2e-tests/`

### Desired workflow/UI wording

- workflow name: `E2E Tests`
- in the short term, job names should refer to the **CLI** side explicitly
- in the longer term, test jobs should name both the **CLI** and the
  **device** side
- docs/logs should refer to **devices** and **platforms** explicitly where
  helpful

### Desired job naming shape

#### Short term

Examples:

- `device-discovery`
- `e2e-tests-macos-cli`
- `e2e-tests-linux-cli`
- later: `e2e-tests-windows-cli`
- `e2e-test-summary`

Display names:

- `Device discovery (macOS CLI)`
- `E2E tests (macOS CLI)`
- `E2E tests (Linux CLI)`
- `E2E test summary`

#### Longer term after splitting jobs per device

Once each job is responsible for exactly one CLI/device pair, the job names
should include both sides.

Examples:

- `e2e-tests-macos-cli-to-mac-device`
- `e2e-tests-macos-cli-to-jetson-device`
- `e2e-tests-macos-cli-to-rpi5-device`
- `e2e-tests-linux-cli-to-jetson-device`
- `e2e-tests-linux-cli-to-rpi5-device`

Display names:

- `E2E tests (macOS CLI -> Mac device)`
- `E2E tests (macOS CLI -> Jetson device)`
- `E2E tests (macOS CLI -> RPi5 device)`
- `E2E tests (Linux CLI -> Jetson device)`
- `E2E tests (Linux CLI -> RPi5 device)`

## Why use E2E tests instead of integration tests

This lane is exercising:

- the real CLI
- real device discovery
- real device agents
- real deploy/build/push/run behavior
- real fixture execution on actual devices

That is better described as **E2E tests** than as generic integration tests.

We should keep “integration tests” available for narrower multi-component tests
that do not cover the full CLI -> device flow.

## Proposed implementation plan

### Phase 1: done

Land the low-risk rename away from `ci-tests` / `test-ci`.

Completed:

- script renamed to `run-integration-tests.sh`
- fixtures moved to `.github/integration-tests/`
- workflow references updated

### Phase 2: rename integration to E2E tests

Rename the remaining top-level artifacts:

- `.github/workflows/integration-tests.yml` -> `.github/workflows/e2e-tests.yml`
- `go/scripts/run-integration-tests.sh` -> `go/scripts/run-e2e-tests.sh`
- `.github/integration-tests/` -> `.github/e2e-tests/`

Update all internal references accordingly.

### Phase 3: clean up workflow wording

Update the workflow labels and descriptions to match the new terminology.

Recommended changes:

- workflow display name:
  - `Hardware Integration Tests` -> `E2E Tests`
- input wording:
  - `platform` description should explicitly say **CLI platform**
- job names:
  - replace ambiguous OS-only wording with `macOS CLI`, `Linux CLI`, etc.
- summary text:
  - `Hardware Integration Test Results` -> `E2E Test Results`

### Phase 4: split the E2E jobs by device

The current structure groups multiple discovered devices under a single CLI job.
That is fine for the first rename, but it prevents the job names from clearly
stating both sides of the test run.

In a second phase, split the workflow so each E2E job covers exactly one
CLI/device pair.

Expected shape:

- one job per CLI/device combination
- job names include both sides
- device selection happens at the workflow/job level rather than as an internal
  fan-out inside one CLI job

Examples:

- `e2e-tests-macos-cli-to-mac-device`
- `e2e-tests-macos-cli-to-jetson-device`
- `e2e-tests-linux-cli-to-rpi5-device`

Benefits:

- job names become self-describing in GitHub Actions
- failures are isolated to one CLI/device lane
- per-device retries and concurrency become easier to reason about
- future platform-specific expansion is easier

### Phase 5: align docs and research notes

Update naming in:

- branch notes
- design docs
- any contributor docs that mention the current integration-test lane

The goal is to make the new terminology the default vocabulary in discussion
and documentation.

## Things to defer for now

These are related, but should not block the naming cleanup:

### 1. Renaming the `platform` input key itself

Inside the workflow, `platform` currently selects the CLI side. Long term we may
want to rename that input to `cli`, but that is a separate compatibility and UX
question.

For now, updating the description to say **CLI platform** is enough.

### 2. Formalizing `target`

A future model may want:

- device + platform = target

But we do not need to encode that in workflow names or docs yet.

### 3. Cleaning up the overloaded meaning of `platform` in `wendy.json`

Today `platform` appears to be used in more than one sense across the repo:

- product-family style values like `wendyos` / `wendy-lite`
- execution-platform style values like `linux` / `darwin`

That is a separate product/config cleanup and should not be mixed into this
rename.

## Acceptance criteria

This cleanup is successful when:

1. all real-device test artifacts use clear names that describe the test lane
   rather than generic CI usage
2. the workflow and related docs consistently use **E2E tests** for this lane
3. job names clearly identify the **CLI** side when relevant
4. discussion/docs use **CLI**, **device**, and **platform** consistently
5. no behavior changes are introduced beyond path/reference updates

## Recommended rollout order

1. land the current `ci-tests` -> `integration-tests` rename
2. follow with the `integration` -> `e2e-tests` rename in one focused change
3. then clean up workflow/job/input/display wording
4. split the E2E jobs so each job maps to a single CLI/device pair
5. defer broader product/config terminology work to a separate branch

## Notes

The important thing is not just shorter names, but clearer concepts.

This branch should move us toward a test taxonomy that can grow with:

- multiple CLIs
- multiple devices
- multiple device platforms

without forcing the naming to be rewritten again as soon as Mac supports both
`darwin` and `linux` platforms.