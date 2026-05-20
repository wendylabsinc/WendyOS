---
name: run-e2e-tests-and-analyze
description: Run WendyAgent Swift E2E tests and analyze AI review instructions against generated command records. Use when asked to run E2E tests, inspect E2E records, evaluate `// AI:` comments, or produce AI review results for WendyAgent E2E tests.
---

# Run E2E Tests and Analyze Records

Use this skill in the `wendy-agent` repository when asked to run Swift E2E
tests and evaluate the `// AI:` review instructions in test files.

## Run the Tests

From the repository root, run the Swift E2E package:

```bash
cd swift/WendyAgentE2ETests
swift test --filter WendyAgentE2ETests
```

For a fresh, easy-to-find record location, prefer setting
`WENDY_AGENT_E2E_TEST_RECORDS_DIR` to an explicit directory:

```bash
cd swift/WendyAgentE2ETests
WENDY_AGENT_E2E_TEST_RECORDS_DIR="$PWD/.build/e2e-test-records.current" swift test --filter WendyAgentE2ETests
```

When `WENDY_AGENT_E2E_TEST_RECORDS_DIR` is set, record files are written
directly into that directory. The harness empties the directory before writing
records for the test process.

Without `WENDY_AGENT_E2E_TEST_RECORDS_DIR`, the harness writes records to a
timestamped UTC directory:

```text
swift/WendyAgentE2ETests/.build/e2e-test-records.YYYY-MM-DD.HH-MM-SS/
```

## Generate the HTML Report

A helper script in this skill folder appends AI review sections to matching
Markdown command records and renders the HTML report from Swift test files,
command records, and the package HTML template:

```bash
.agents/skills/run-e2e-tests-and-analyze/render-e2e-report.py \
  --records-dir swift/WendyAgentE2ETests/.build/e2e-test-records.current
```

By default, the script writes `index.html` into the records directory and updates
matching `*.md` command records only when there is something noteworthy for a
human to review. Each appended `AI review` section includes the full source
`// AI:` comment block first, followed by a prose Markdown report. Passing,
unsurprising checklist items should not generate a report. Existing generated AI
review sections are replaced, so rerunning the script is idempotent. The HTML
`AI` filter matches only tests with an actual generated report, not every test
that merely has a `// AI:` checklist. When the `AI` filter is active, the
Markdown report appears verbatim in a fixed-width block under each matching test
row. Use `--no-append-ai-to-records` to render HTML without touching Markdown
records. Use `--include-fake-analysis` to add deterministic fake prose reports
for UI testing. Override paths with `--package-dir`, `--tests-dir`, `--template`,
`--records-dir`, and `--output` when needed.

## Locate Records

After the run, locate the records directory.

If `WENDY_AGENT_E2E_TEST_RECORDS_DIR` was set, use that directory directly.
Otherwise, find the newest timestamped records directory:

```bash
find swift/WendyAgentE2ETests/.build -maxdepth 1 -type d -name 'e2e-test-records.*' | sort | tail -1
```

Each test record is a Markdown file named:

```text
<TestFileNameWithoutSwift>.<test-function-slug>.md
```

Example:

```text
CLIBasicsTests.wendy-version-prints-the-cli-version.md
```

A single test may run multiple commands; those command records are appended in
the same file and separated with Markdown `---` rules.

## Analyze Test Files One by One

1. Enumerate only the WendyAgent E2E test files:

   ```bash
   find swift/WendyAgentE2ETests/Tests/WendyAgentE2ETests -name '*Tests.swift' | sort
   ```

2. Read one test file completely.

3. For each `@Test` function:
   - Find the `// AI:` checklist inside that test.
   - If there is no `// AI:` section, skip AI evaluation for that test.
   - Identify the matching record file in the newest records directory.
   - Read the full record file.
   - Compare the captured stdout/stderr and command metadata against each
     checklist item.

4. Evaluate each checklist item with status emojis:
   - `✅ pass` — the record clearly satisfies the instruction.
   - `⚠️ concern` — the record is ambiguous, noisy, incomplete, or surprising.
   - `❌ fail` — the record contradicts the instruction.

   Prefer these symbols over colored hearts because they are more explicit in
   plain-text logs and easier to scan in Markdown.

5. Continue test-by-test until every `// AI:` section has been evaluated.

## Matching Tips

- The record file name uses the test file name without `.swift` verbatim, then a
  slug of the test function name.
- If the exact slug is unclear, list the records and match by file prefix:

  ```bash
  find <records-dir> -maxdepth 1 -type f -name 'CLIBasicsTests.*.md' | sort
  ```

- Commands executed by setup helpers, such as `Machine+WendyAgentE2ETests`, may
  have their own records. Treat those as setup evidence unless the test's
  `// AI:` instructions explicitly refer to setup behavior.

## Report Format

Summarize results by test file and test function:

```markdown
## CLIBasicsTests.swift

### 'wendy --help' describes the top-level command groups

Record: `CLIBasicsTests.wendy-help-describes-the-top-level-command-groups.md`

- ✅ pass: Help text is readable and well-grouped.
- ✅ pass: Group names match the CLI docs.

Notes: No stderr output was captured.
```

Include concise evidence for any `⚠️ concern` or `❌ fail`, quoting only the
relevant record lines.
