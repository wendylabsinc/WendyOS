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

### Iteration 3: show aggregate duration ranges

The top-level report should summarize duration as a range across all concrete
observations for each test. Once this lands, the duration display and duration
bar are no longer hardcoded placeholders.

Duration collection:

- For each test row, read durations from all matching observations under
  `<suite-key>/<test-key>/<target-name>/<attempt>/test-results.xml`.
- Compute the minimum and maximum duration from observations that include a
  valid duration.
- Ignore missing durations for the range calculation.
- If no observations include a valid duration, render the existing empty
  duration state.

Duration text display rules:

- If there is exactly one valid duration, render that single formatted value,
  such as `0.4s`.
- If multiple valid durations format to the same value, render that single
  formatted value.
- If the minimum and maximum differ, render a range, such as `0.2s–1.8s`.

Duration bar semantics:

- Keep the existing duration scale and duration-to-color mapping.
- Treat the full bar track as the global duration scale.
- Normalize `minDuration` and `maxDuration` onto that scale.
- Render the filled segment from the normalized min position to the normalized
  max position.
- Color the segment as a gradient from the min-duration color to the
  max-duration color.
- Leave the track before the min position and after the max position unfilled.

Single-point display:

- If `minDuration == maxDuration`, render a small visible segment or marker at
  the normalized duration position so single-observation or stable-duration
  tests remain visible.
- The marker should use the color for that duration.

This makes the aggregate report communicate both absolute runtime and runtime
variance across targets and attempts.

### Iteration 4: expand tests inline with target and attempt observations — done

The top-level report should expand test rows inline instead of linking to a
dedicated per-test page. The placeholder per-test pages should be removed for
now.

Top-level behavior:

- Render each test as an expandable row, similar to the pre-aggregate report.
- Keep the collapsed row focused on the aggregate summary:
  - target outcome badges from iteration 2
  - duration range from iteration 3
  - filters and search remain top-level report controls
- Clicking the test row expands or collapses inline details.
- Do not generate or link `<suite-key>/<test-key>/index.html` placeholder pages
  in this iteration.

Expanded content:

- Show a sublist of concrete observations grouped by target.
- Each observation row represents one `<target-name>/<attempt>` for that test.
- Attempts should be visually grouped by target.
- Show the target name only on the first row for a target group; subsequent
  attempt rows in the same target group should leave the target column blank or
  use an equivalent visual continuation.
- Show the attempt number on every observation row.

Observation row fields:

- Target name, only for the first attempt in the target group.
- Attempt number.
- Observation status badge: `Passed`, `Failed`, `Skipped`, or `Unknown`.
- Single-observation duration text.
- Single-observation duration bar using the existing pre-aggregate behavior:
  fill width and color are based on that observation's duration.

Status semantics:

- Observation rows do not use `Flaked`; flaking is a target-level aggregate
  outcome derived from multiple attempts.
- A concrete attempt can only render as passed, failed, skipped, or unknown.

This makes the aggregate report useful without requiring a separate detailed
per-test page while preserving the future option to add one later.

Completed follow-up pieces:

- Expanded rows show the concrete target name and a separate CLI-to-agent route
  column using platform icons.
- Expanded rows link directly to per-observation `recording.sh.txt` and
  `recording.md` via hover-only Shell and Record buttons.
- Aggregate, suite, and test AI review summaries are rendered as Markdown in
  the report when `review.summary.md` is present, with links to
  `review.details.md`.

### Iteration 5: generate aggregate AI reviews in two stages

Reintroduce AI review generation for aggregate roots as a two-stage workflow.
Review generation remains separate from deterministic test execution and report
rendering.

CLI shape:

```text
swift-e2e-testing review --run-dir <aggregate-dir> \
  --suite-review-prompt Support/e2e-review-suite.prompt.md \
  --report-review-prompt Support/e2e-review-report.prompt.md
```

Useful options:

```text
--stage suites|report|all
--overwrite
--provider <provider>
--model <model>
```

Default stage should be `all`. Prompt paths should be configurable via CLI
options and should default to files under `Support/`.

Prompt files:

- `Support/e2e-review-suite.prompt.md`
- `Support/e2e-review-report.prompt.md`

Stage 1: suite-scoped review

- Run one AI agent per suite, in parallel.
- Keep all context for a suite in one agent session.
- The suite prompt is responsible for deciding whether to write:
  - zero or more per-test paired reviews at
    `<aggregate>/<suite>/<test>/review.summary.md` and
    `<aggregate>/<suite>/<test>/review.details.md`
  - an optional paired suite review at `<aggregate>/<suite>/review.summary.md`
    and `<aggregate>/<suite>/review.details.md`
- If nothing is noteworthy for a test, neither per-test review file should be
  written.
- If nothing is noteworthy for the suite as a whole, neither suite review file
  should be written.
- Inputs should include:
  - suite source/tests and `// AI:` comments
  - aggregate test outcome counts for every test in the suite
  - concrete target/attempt observations for every test in the suite
  - status, duration, target route, and artifact paths for each observation
  - relevant snippets or summaries from `recording.md` / `recording.sh.txt`
    where needed, without recursively scanning copied sandboxes
  - existing test/suite reviews when `--overwrite` is false
- The suite prompt should ask for two files when a review is warranted:
  - `review.summary.md`: a very concise Markdown bullet list of clear,
    actionable findings. It must not include status/severity, headings, labels,
    or prose paragraphs.
  - `review.details.md`: supporting evidence and action items. It must not
    include status/severity.
- Suite-level summaries should only include suite-level or cross-test actions;
  they should not repeat per-test findings already covered at lower scope.
- `review.summary.md` should be suitable for inline display, for example:

  ```md
  - Seed cache fixtures across table and JSON tests so populated-list behavior is actually exercised.
  ```

Stage 2: report-level review

- Run one AI agent after suite reviews complete.
- The report prompt writes `<aggregate>/review.summary.md` and
  `<aggregate>/review.details.md` only when there is an actionable
  aggregate-level or cross-suite finding.
- Inputs should include:
  - aggregate-wide status summary
  - failed/flaked/skipped target summaries
  - suite reviews generated in stage 1
  - per-test reviews generated in stage 1
  - links or relative paths to relevant suite/test details
- The report summary should be only a concise bullet list of aggregate-level
  actions. It should not repeat suite/test findings already covered at lower
  levels or restate obvious counts/statuses visible in the report. Details can
  include evidence and links as needed.
- The report review should run even if no suite/test reviews were generated,
  because aggregate failures/flakes/skips may still require a top-level summary.

Filesystem constraints:

- Avoid recursive filesystem scans over aggregate roots.
- Walk only the canonical aggregate depth:

  ```text
  <aggregate>/<suite>/<test>/<target>/<attempt>/...
  ```

- Do not scan inside copied `cli/` or `agent/` sandboxes.

Integration:

- Keep `test`, `aggregate`, `review`, and `report` as explicit steps.
- Once implemented, re-enable aggregate review in shell wrappers, make targets,
  and CI by composing:

  ```text
  test → aggregate → review → report
  ```

## Open plumbing questions

We still need to decide implementation details:

1. Whether `aggregate`, `review`, and `report` live entirely in `swift-e2e-testing`, shell wrappers, or both.
2. Whether raw CI artifacts are uploaded as individual runs, aggregate only, or both during transition.
3. How local `make` targets should expose each step and composed workflows such as `e2e-run`.
