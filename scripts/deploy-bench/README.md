# Deploy benchmark (WDY-3210)

Repeatable `wendy run` scenarios for measuring the deploy pipeline before and
after each WDY-3210 PR. Each run records wall time, the CLI's `[timing]`
phases (`WENDY_TIMING=1`), the push summary line, and the agent's phase logs.

## Prerequisites

- A device on agent `2026.09.16-215446` or newer (older agents have the
  WDY-3113 SPDP storm, which skews timings for 120 s of every 7 minutes).
- The CLI under test on `PATH` as `wendy`.
- `perl` (macOS and Linux ship it).

## Usage

    scripts/deploy-bench/run.sh <device> <scenario> [runs]

| Scenario | What changes per run | Compare |
| -- | -- | -- |
| `big-layer` | A ~430 MB compressible layer whose every chunk is new | upload phase, device prepare |
| `apt-layer` | One more apt package in a ~330 MB `build-essential` layer | runs 2+ vs run 1 (cold) |
| `tiny-layers` | 20 tiny layers | push + prepare |
| `one-line` | One Python source line | device-side time across many runs |

Run each scenario at least 3 times; the first `big-layer` and `apt-layer` run
also pushes their base layers, so compare runs 2 and later. Results land in
`scripts/deploy-bench/results/` (git-ignored). Remove the bench apps from the
device afterwards with `wendy device apps remove <appId>`.
