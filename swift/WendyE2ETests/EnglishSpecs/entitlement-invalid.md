# Rejecting an unknown entitlement leaves the project unchanged

## Setup

Use a fresh isolated CLI home, temporary directory and working directory. No
authentication or running agent is needed. Create `wendy.json` with exactly
`{"appId":"sh.wendy.demo"}` followed by a newline. Preserve its original bytes
for comparison.

## Action

Run the real CLI command `wendy project entitlements add nonsense` without an
interactive terminal, then read `wendy.json` back from disk.

## Expectations

The command exits with failure and writes nothing to stdout. The human-readable
stderr diagnostic names the rejected `nonsense` type and includes `network`
among the valid choices. The project file has exactly its original bytes, remains
valid JSON with the original app ID, and has no `entitlements` key. A failure
status alone does not establish that the project was preserved.

## Cleanup

Use the existing scenario's cleanup-safe lifetime. Remove the case's
`wendy.json` even after failure. Preserve command recordings and the isolated
run directory. Do not change any other project.
