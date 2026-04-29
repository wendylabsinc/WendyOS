# WendyAgentE2ETests

Minimal Swift E2E scaffolding built around an SSH-only `Machine` helper.

## Run tests

```bash
cd swift/WendyAgentE2ETests
swift test
```

## Machine configuration

`Machine` takes the SSH target and remote working directory separately:

```swift
let machine = try Machine(ssh: "user@host", path: "/path/to/repo")
```

Each command runs in its own SSH invocation.

## Run the smoke test

The smoke test is gated behind `WENDY_E2E_SMOKE=1` and requires an SSH target
and remote working directory:

```bash
cd swift/WendyAgentE2ETests
WENDY_E2E_SMOKE=1 \
E2E_MACHINE_SSH='user@host' \
E2E_MACHINE_PATH='/path/to/wendy-agent' \
swift test --filter MachineSmokeTests
```
