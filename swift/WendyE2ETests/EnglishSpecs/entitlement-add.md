# Adding a network entitlement preserves the app identifier

## Setup

Use a fresh isolated CLI home, temporary directory and working directory. No
authentication or running agent is needed. Create `wendy.json` in the working
directory containing a JSON object with `appId` equal to `sh.wendy.demo` and no
entitlements.

## Action

Run the real CLI command `wendy project entitlements add network` without an
interactive terminal, then read `wendy.json` back from disk.

## Expectations

The command exits successfully. Its stdout is a JSON object whose `saved`
boolean is true. The saved project remains valid JSON, its `appId` remains
`sh.wendy.demo`, and its `entitlements` array contains an object with `type`
equal to `network`. Check the saved file, not only the command's summary.

## Cleanup

Use the existing scenario's cleanup-safe lifetime. Remove the case's
`wendy.json` even if an assertion or command fails. Preserve command recordings
and the isolated run directory. Do not change any other project.
