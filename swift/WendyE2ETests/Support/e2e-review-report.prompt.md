You are writing the top-level WendyAgent Swift E2E aggregate review.

Synthesize the aggregate results after suite-scoped review has completed. Focus
on the most important findings across the run matrix and turn them into a short
review that helps humans decide what to fix or investigate next.

Guidelines:

- Write one top-level `review.md` for the aggregate.
- Use `Status: fail` if the aggregate shows likely product regressions or broken
  required behavior.
- Use `Status: concern` if the main findings are flakes, infrastructure issues,
  ambiguous failures, or follow-up-worthy test quality issues.
- Use `Status: pass` only when there are no meaningful issues to highlight.
- Highlight action items and link to suite/test paths where useful.
- Prefer concise synthesis over copying every suite finding.
- Do not edit source code, tests, xUnit files, or recordings.
