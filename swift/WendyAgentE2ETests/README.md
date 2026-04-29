# WendyAgentE2ETests

Minimal Swift E2E scaffolding built around a tiny `Machine` helper.

## Run unit tests

```bash
cd swift/WendyAgentE2ETests
swift test
```

## Run the smoke test

The smoke test is gated behind `WENDY_E2E_SMOKE=1`.

```bash
cd swift/WendyAgentE2ETests
WENDY_E2E_SMOKE=1 swift test --filter MachineSmokeTests
```

Optional machine specs:

- `E2E_RUNNER`
- `E2E_CLI`
- `E2E_AGENT`
- `E2E_DEVICE`

Machine spec format:

- `local:/absolute/path`
- `user@host:/path/to/workdir`

Without explicit `E2E_CLI` or `E2E_AGENT`, the smoke test creates local temporary directories and pushes the built artifacts there.
