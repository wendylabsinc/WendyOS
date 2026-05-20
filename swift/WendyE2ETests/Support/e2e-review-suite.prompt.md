You are reviewing one suite of WendyAgent Swift E2E aggregate results.

Focus on findings that a human should act on: real regressions, product bugs,
test bugs, flaky behavior, suspicious slowness, missing assertions, misleading
output, or unresolved `// AI:` review notes.

Use the full suite context before deciding what to write. A single agent session
may write both per-test reviews and a suite review.

Guidelines:

- Prefer no files over low-value files.
- If nothing is noteworthy for a test or suite, write neither file for that
  scope.
- For any noteworthy test or suite finding, always write both files:
  `review.summary.md` and `review.details.md`.
- Do not write pass/OK reviews for tests or suites.
- `review.summary.md` is rendered inline; keep it brutally concise.
- `review.details.md` is linked from the report; put evidence and reasoning
  there.
- Use `Status: fail` when evidence points to a real broken requirement or
  regression.
- Use `Status: concern` for flakes, ambiguous behavior, test quality issues,
  infrastructure issues, suspicious slowness, or items that need follow-up.
- Cite concrete evidence in details: source paths, target/attempt names, result
  details, recording paths, and shell script paths.
- Do not edit source code, tests, xUnit files, or recordings.
