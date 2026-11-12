Synthetic demonstration data, not model performance results.

# Agent evaluation comparison

Verdict: regression

Success intervals are 95% per comparison. A pass excludes a success-rate loss larger than 10%, with at least 10 attempts per side.

| Task | Agent | Target | Baseline R/W/M/A | Candidate R/W/M/A | Tokens (b → c) | Seconds (b → c) | Successes (b / c) | Success loss, 95% interval | Verdict / flags |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| deploy-http | claude | sim | 40R/0W/0M/0A of 40 | 40R/0W/0M/0A of 40 | 10000 → 10000 | 50 → 50 | 40/40 / 0/40 | [87.6%, 100.0%] | regression: success_rate |
| repair-startup | claude | sim | 40R/0W/0M/0A of 40 | 40R/0W/0M/0A of 40 | 10000 → 10000 | 50 → 50 | 40/40 / 40/40 | [-8.8%, 8.8%] | pass:  |
| update-http | claude | sim | 3R/0W/0M/0A of 3 | 3R/0W/0M/0A of 3 | 10000 → 10000 | 50 → 50 | 3/3 / 3/3 | [-56.1%, 56.1%] | insufficient_evidence: insufficient: success_rate |
