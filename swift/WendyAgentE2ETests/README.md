# WendyE2ETesting

Minimal Swift E2E scaffolding built around a local-or-SSH `Machine` helper.

## Run tests

```bash
cd swift/WendyE2ETesting
swift test
```

## Machine configuration

`Machine` takes an optional SSH target and optional working directory:

```swift
let remote = Machine(name: "Remote", ssh: "user@host", path: "/path/to/repo")
let local = Machine(name: "Local", path: "/path/to/repo")
```

If `ssh` is omitted, commands run on the local machine with `path` as their
working directory, defaulting to the current directory. If remote `path` is
omitted, commands run in the SSH user's home directory. Each remote command runs
in its own SSH invocation. Console output is prefixed with the machine name.

## Run the simple command test

The simple command test runs locally by default:

```bash
cd swift/WendyE2ETesting
swift test --filter MachineTests/runsSimpleCommand
```
