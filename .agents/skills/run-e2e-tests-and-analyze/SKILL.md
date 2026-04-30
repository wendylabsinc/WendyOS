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
swift test
```

For a fresh, easy-to-find record location, prefer setting
`WENDY_AGENT_E2E_TEST_RECORDS_DIR` to an explicit directory:

```bash
cd swift/WendyAgentE2ETests
rm -rf .build/e2e-test-records.*
WENDY_AGENT_E2E_TEST_RECORDS_DIR="$PWD/.build" swift test
```

The harness writes records to a timestamped UTC directory:

```text
swift/WendyAgentE2ETests/.build/e2e-test-records.YYYY-MM-DD.HH-MM-SS/
```

If `WENDY_AGENT_E2E_TEST_RECORDS_DIR` is set, the timestamped records folder is
created under that directory instead.

## Locate Records

After the run, find the newest records directory:

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

1. Enumerate test files, usually:

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

4. Evaluate each checklist item as:
   - `pass` — the record clearly satisfies the instruction.
   - `concern` — the record is ambiguous, noisy, incomplete, or surprising.
   - `fail` — the record contradicts the instruction.

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

- pass: Help text is readable and well-grouped.
- pass: Group names match the CLI docs.

Notes: No stderr output was captured.
```

Include concise evidence for any `concern` or `fail`, quoting only the relevant
record lines.
