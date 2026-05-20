# Swift E2E aggregate plan

## Current run ID shape

Individual E2E runs use this logical ID shape:

```text
<workflow-name>.<run-id>.<target-name>.<attempt>
```

Examples:

```text
swift-e2e-tests.local260520.macos-26-to-jetson-orin-nano.0001
swift-e2e-tests.gh1234567890.macos-26-to-jetson-orin-nano.0001
```

Parts:

- `workflow-name`: the spec/workflow family, currently `swift-e2e-tests`
- `run-id`: `local<yy><mm><dd>` locally, or `gh<github-run-id>` in GitHub Actions
- `target-name`: one concrete E2E target, such as `macos-26-to-jetson-orin-nano`
- `attempt`: four-digit attempt number, such as `0001`

## Raw individual run shape

A raw individual run remains records-only:

```text
<run-id>/
  info.json
  test-results.xml
  tests/
    <suite-key>/
      <test-key>/
        recording.md
        recording.sh.txt
        review.md        # optional
        cli/
        agent/
```

No binaries are stored in the run directory. Reports are rendered only from aggregate directories.

## Canonical aggregate shape

The aggregate layout transposes individual runs into a test-first hierarchy:

```text
<workflow-name>.<run-id>/
  info.json
  index.html
  review.md              # aggregate-level review/actions

  <suite-key>/
    info.json
    review.md            # optional suite-level review

    <test-key>/
      info.json
      review.md          # cross-target/cross-attempt test review

      <target-name>/
        info.json
        review.md        # optional target-specific review for this test

        <attempt>/
          info.json
          review.md      # optional exact-observation review
          recording.md
          recording.sh.txt
          cli/
          agent/
```

Example:

```text
swift-e2e-tests.local260520/
  wendy-completion-zsh/
    prints-command-help/
      macos-26-to-jetson-orin-nano/
        0001/
          info.json
          recording.md
          recording.sh.txt
          cli/
            home/
            tmp/
```

This is isomorphic with a target-first view:

```text
<workflow-name>.<run-id>/<target-name>/<attempt>/<suite-key>/<test-key>
```

but the canonical aggregate is test-first because it is better for review:

- root `review.md`: whole run/matrix summary
- suite `review.md`: suite-wide concerns
- test `review.md`: cross-target/cross-attempt test concerns
- target `review.md`: target-specific behavior for that test
- attempt `review.md`: exact recorded observation concerns

## Integration rule

Given a raw run ID:

```text
<workflow-name>.<run-id>.<target-name>.<attempt>
```

and a raw test directory:

```text
<raw-run>/tests/<suite-key>/<test-key>/
```

integrate it into the aggregate at:

```text
<workflow-name>.<run-id>/<suite-key>/<test-key>/<target-name>/<attempt>/
```

The raw files copied/moved into that observation directory are:

```text
recording.md
recording.sh.txt
review.md        # if present
cli/
agent/
```

Run-level files such as `info.json` and `test-results.xml` should be folded into scoped `info.json` files at the aggregate root, suite, test, target, or attempt level as appropriate.

## Command model

The E2E workflow should be split into four explicit commands/steps:

1. `test`
   - Runs deterministic Swift E2E tests.
   - Produces one raw individual run directory.
   - Does not review, aggregate, or render reports.

2. `aggregate`
   - Reads one or more raw individual run directories.
   - Parses each run ID into `<workflow-name>`, `<run-id>`, `<target-name>`, and `<attempt>`.
   - Transposes raw run artifacts into the canonical aggregate layout.
   - Writes deterministic `info.json` files at the aggregate, suite, test, target, and attempt levels.

3. `review`
   - Reads an aggregate directory.
   - Writes scoped `review.md` files at the appropriate levels.
   - Keeps review separate from deterministic test execution.

4. `report`
   - Reads an aggregate directory.
   - Writes the single aggregate `index.html`.
   - Covers both one-run and multi-run aggregates.

`make` targets and CI jobs should compose these steps rather than combining their responsibilities inside the test step.

## Implementation iterations

### Iteration 1: transpose existing per-run artifacts — done

The first implementation maps the current raw run output onto the aggregate hierarchy without changing the per-run review and report behavior.

`aggregate` should be fully implemented for the current artifact set:

- Read one or more raw run directories.
- Parse each raw run ID into `<workflow-name>`, `<run-id>`, `<target-name>`, and `<attempt>`.
- Create the aggregate root `<workflow-name>.<run-id>/`.
- Place each raw run under the test-first aggregate path:

  ```text
  <workflow-name>.<run-id>/<suite-key>/<test-key>/<target-name>/<attempt>/
  ```

- Preserve the current per-run files in that mapped location, including `recording.md`, `recording.sh.txt`, `review.md`, `cli/`, and `agent/`.

`review` is disabled while the aggregate report flow is being reshaped:

- Do not write per-observation `review.md` files from aggregate runs for now.
- Reintroduce review later as cross-target/cross-attempt review, not per-observation review.

`report` is now aggregate-first:

- Render a single top-level aggregate `index.html` under `<workflow-name>.<run-id>/`.
- Keep the suite-grouped report UI with filters and search.
- Each test row links to `<suite-key>/<test-key>/index.html` instead of expanding inline.
- Per-test pages are placeholders that say `Not implemented yet` until the detailed test view is designed.
- Per-test Shell, Record, and Report buttons are not rendered.

This gives us a complete aggregate command and makes the aggregate report the canonical report surface.

Completed pieces:

- `swift-e2e-testing aggregate` writes the test-first aggregate hierarchy.
- `Scripts/E2EAggregate.sh` wraps the aggregate command.
- Aggregate AI review is disabled for now.
- `Scripts/E2EReport.sh` renders the aggregate `index.html` directly.
- Local `make e2e-run*` composes test → aggregate → review → report.
- CI matrix jobs upload raw runs, and the aggregate job downloads, aggregates, reviews, reports, and uploads the aggregate.

### Iteration 2: count target outcomes in the aggregate report

The top-level report should summarize each test across targets instead of
collapsing it to a single status. Status is computed in two stages:

1. Attempt outcomes are reduced to one target outcome for each
   `<suite-key>/<test-key>/<target-name>`.
2. Target outcomes are counted and rendered as badges on the test row.

Per-target status semantics:

- If all attempts for a target for a test pass, the target counts as `passed`.
- If some attempts for a target for a test pass and some fail, the target
  counts as `flaked`.
- If all attempts for a target for a test fail, the target counts as `failed`.
- If a test is skipped for a target, the target counts as `skipped`.

Top-level test rows render one badge per non-zero target-status bucket:

- `Passed N`: number of targets whose target outcome is passed.
- `Flaked N`: number of targets whose target outcome is flaked.
- `Skipped N`: number of targets whose target outcome is skipped.
- `Failed N`: number of targets whose target outcome is failed.

Badge number display rules:

- If a test has exactly one non-zero target-status bucket and the count is one,
  render only the label, such as `Passed` or `Failed`.
- If a test has exactly one non-zero target-status bucket and the count is
  greater than one, render the count, such as `Passed 3`.
- If a test has multiple non-zero target-status buckets, render counts on every
  badge, such as `Passed 2`, `Flaked 1`, and `Failed 1`.

Visual treatment:

- Keep passed green and failed red.
- Add a new `flaked` badge in orange.
- Keep skipped yellow/amber, but adjust its tone so it is clearly distinct from
  the orange flaked badge.

Filtering should treat the row as multi-status:

- The `Passed` filter shows tests with a non-zero passed target count.
- The `Flaked` filter shows tests with a non-zero flaked target count.
- The `Skipped` filter shows tests with a non-zero skipped target count.
- The `Failed` filter shows tests with a non-zero failed target count.

Default filter priority should surface actionable results first:

1. failed
2. flaked
3. AI review, once aggregate review is reintroduced
4. all

## Open plumbing questions

We still need to decide implementation details:

1. Whether `aggregate`, `review`, and `report` live entirely in `swift-e2e-testing`, shell wrappers, or both.
2. Whether raw CI artifacts are uploaded as individual runs, aggregate only, or both during transition.
3. How local `make` targets should expose each step and composed workflows such as `e2e-run`.
