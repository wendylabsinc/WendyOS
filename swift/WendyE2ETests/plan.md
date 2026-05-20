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

No binaries are stored in the run directory. No per-run `report.html` is required once aggregate rendering exists.

## Canonical aggregate shape

The aggregate layout transposes individual runs into a test-first hierarchy:

```text
<workflow-name>.<run-id>/
  info.json
  report.html
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

## Open plumbing questions

We still need to decide how to split this across scripts and tools:

1. Which command creates the aggregate directory and transposes raw runs?
2. Which command writes deterministic `info.json` files at each level?
3. Whether raw CI artifacts are uploaded as individual runs, aggregate only, or both during transition.
4. Whether report rendering reads raw runs, aggregate layout, or both.
5. Whether AI review runs before or after transposition.
6. How local `make` targets should expose: test-only, aggregate, review, and report steps.
