# Setting a default device preserves unrelated known configuration

## Setup

Use a fresh isolated CLI home, temporary directory and working directory. No
authentication or running agent is needed. In that home, create
`.wendy/config.json` with these values:

- `analytics.enabled` is false.
- `lastCLIUpdateCheck` is `2026-07-20T12:00:00Z`.
- `completionPromptDismissed` is true.

No real credentials belong in this fixture.

## Action

Run the real CLI command `wendy device set-default 127.0.0.1:1` without an
interactive terminal. Read `.wendy/config.json` back from the isolated home.

## Expectations

The command exits successfully. The saved configuration is valid JSON and its
`defaultDevice` value is `127.0.0.1:1`. `analytics.enabled` remains false,
`lastCLIUpdateCheck` remains exactly `2026-07-20T12:00:00Z`, and
`completionPromptDismissed` remains true. Inspect the saved values rather than
inferring persistence from a success message. This action saves a hostname; it
does not require connecting to the address.

## Cleanup

Use the existing scenario's cleanup-safe lifetime. Remove this case's config
file even after failure. Preserve command recordings and the isolated run
directory. Never read or modify the user's real config.
