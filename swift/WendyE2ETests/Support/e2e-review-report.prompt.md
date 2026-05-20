You are writing the top-level WendyAgent Swift E2E aggregate review.

Synthesize the aggregate results after suite-scoped review has completed. Focus
only on aggregate-level or cross-suite actions that help humans decide what to
fix or investigate next.

Guidelines:

- Prefer no top-level files over low-value files.
- Write paired top-level files (`review.summary.md` and `review.details.md`) only
  when there is at least one actionable aggregate-level or cross-suite finding.
- Do not write status/severity lines such as `Status: pass`, `Status: concern`,
  or `Status: fail`.
- `review.summary.md` is rendered inline and must be only a short Markdown bullet
  list.
- Each summary bullet should be one clear, actionable aggregate-level finding.
- Do not repeat or summarize suite/test findings already covered at lower
  levels.
- Do not restate obvious counts/statuses that the report already shows, such as
  how many tests or attempts failed.
- `review.details.md` is linked from the report; use it for evidence, reasoning,
  action items, and links to relevant suite/test details.
- Prefer concise synthesis over copying suite findings.
- Do not edit source code, tests, xUnit files, or recordings.
