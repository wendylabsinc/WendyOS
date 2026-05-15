---
name: e2e-analyze-ci
description: Fetch the latest completed Swift E2E CI artifacts for the current branch or PR, analyze // AI: comments and failed tests, and write per-test ai-analysis.md files next to recording.md.
---

# Analyze Swift E2E CI Artifacts

Use this skill in the `wendy-agent` repository when asked to analyze the latest
Swift E2E CI report, analyze `// AI:` comments from CI artifacts, or add
per-test `ai-analysis.md` files that the HTML report can render inline.

## Goal

1. Determine the current git branch and associated PR, if any.
2. Fetch artifacts from the latest **completed** `Swift E2E Tests` workflow run
   for that branch/PR.
3. Analyze every test that has one or more `// AI:` comment blocks in source.
4. Also investigate every failed test, even if it has no `// AI:` comments.
5. Write one `ai-analysis.md` beside each analyzed test's `recording.md`.
6. Regenerate `report.html` so analyzed tests receive the existing `AI` badge and
   show the analysis inline when expanded.

## Prerequisites

- `gh` must be authenticated with access to the repository and workflow
  artifacts.
- `jq` and `unzip` are useful for inspection, though `gh run download` can
  unpack artifacts directly.
- Run commands from the repository root unless noted otherwise.

## Fetch the Latest Completed CI Artifacts

Use the helper script in this skill directory:

```bash
.agents/skills/e2e-analyze-ci/fetch-latest-swift-e2e-artifacts.sh
```

The helper determines the current branch, looks up the associated PR when one is
visible to `gh`, finds the latest completed `swift-e2e-tests.yml` run for that
branch, and downloads `wendy-e2e-*` artifacts into:

```text
swift/Build/e2e-ci-analysis/run-<run-id>/artifacts/
```

It also writes run metadata to:

```text
swift/Build/e2e-ci-analysis/run-<run-id>/metadata.json
```

Useful overrides:

```bash
.agents/skills/e2e-analyze-ci/fetch-latest-swift-e2e-artifacts.sh \
  --repo wendylabsinc/wendy-agent \
  --branch kb.swift-e2e-tests
```

Each artifact should contain files like:

```text
README.md
info.json
report.html
test-results-swift-testing.xml
tests/<test-slug>/recording.md
tests/<test-slug>/recording.sh.txt
```

## Build the Test Map

For each artifact directory, use the recording source locations to map records
back to test source:

```bash
find "$out/artifacts" -path '*/tests/*/recording.md' -type f | sort
```

Each command section in a recording includes a source line:

```text
- Source: `/path/to/WendyFooTests.swift:123`
```

Open that test file in the current checkout and identify the enclosing `@Test`
function. Only treat comments beginning with `// AI:` inside that test function
as AI analysis instructions. A test can contain multiple `// AI:` blocks,
separated by code or blank lines. These blocks are prompts, notes, or
instructions; they are not necessarily checklists. Examples:

```swift
// AI: Confirm the output is actionable for a human operator.

// AI:
// Also check that stderr does not leak implementation details.
```

## Analysis Rules

Analyze a test when either condition is true:

- The test function has one or more `// AI:` comment blocks.
- The xUnit results or report show the test failed.

For `// AI:` tests, compare the instructions or notes to the captured command
evidence in `recording.md` and `recording.sh.txt`, using the whole test source
as context.

For failed tests, inspect:

- `test-results-swift-testing.xml` failure text
- the test's `recording.md`
- stdout/stderr/status for each command
- the test source around the failing assertion, when source line information is
  available

Use these result words consistently:

- `pass` — evidence clearly satisfies the `// AI:` instruction.
- `concern` — evidence is ambiguous, noisy, incomplete, flaky, or surprising.
- `fail` — evidence contradicts the `// AI:` instruction or the test failure appears
  product-related.

## Run the Swift Analyzer

After downloading artifacts, run the checked-in analyzer from `swift/`:

```bash
cd swift
for run_dir in Build/e2e-ci-analysis/run-*/artifacts/wendy-e2e-*; do
  [ -d "$run_dir/tests" ] || continue
  bash Scripts/E2EAnalyze.sh --run-dir "$run_dir" --provider auto
done
```

Use `--provider anthropic` (or `--provider claude`) with `ANTHROPIC_API_KEY`,
or `--provider openai` with `OPENAI_API_KEY`, to force a provider. Use
`--overwrite` to replace existing per-test outputs.

## Per-Test Analysis File

The analyzer writes analysis next to the recording:

```text
.../tests/<test-slug>/ai-analysis.md
```

Use this format:

```markdown
# AI Analysis

Status: pass|concern|fail
Source: `WendyFooTests.swift:<line>`
Record: `recording.md`

## AI comments

- pass|concern|fail: <AI instruction or note>
  Evidence: <brief quote or summary from the recording>

## Failure investigation

Only include this section for failed tests. Explain the likely cause and whether
it appears to be a product bug, test bug, infrastructure issue, or unknown.

## Notes

Optional concise context for a human reader.
```

Keep the analysis short. Quote only the evidence needed to justify concerns or
failures. If a `// AI:` instruction passes with unsurprising evidence, one or two
sentences are enough.

## Regenerate HTML Reports

After writing per-test analysis files, regenerate each artifact's HTML report from
`swift/`:

```bash
cd swift
for run_dir in Build/e2e-ci-analysis/run-*/artifacts/wendy-e2e-*; do
  [ -d "$run_dir/tests" ] || continue
  bash Scripts/E2EReport.sh --run-dir "$run_dir"
done
```

The Swift report renderer reads `tests/<test-slug>/ai-analysis.md`. Analyzed tests
are tagged with the existing black `AI` badge, participate in the `AI` filter,
and display the analysis inline in the expanded test details next to the command
execution.

## Final Response

Summarize:

- branch and PR analyzed
- workflow run URL and conclusion
- artifact directories analyzed
- number of tests with `// AI:` comments analyzed
- number of failed tests investigated
- where the regenerated `report.html` files were written
- any `concern` or `fail` findings
