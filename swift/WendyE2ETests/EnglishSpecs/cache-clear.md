# Cache clear removes cached entries and remains safe to repeat

## Setup

Use a fresh isolated CLI home, temporary directory and working directory. No
authentication or running agent is needed. In the CLI's platform-specific Wendy
cache directory, create an `images` directory containing `entry.bin` with the
text `cached` followed by a newline.

## Action

Run the real CLI command `wendy cache clear` without an interactive terminal.
Then run it a second time after checking the first result.

## Expectations

Each invocation exits successfully, prints exactly `Cache cleared.` followed by
a newline on stdout, and prints nothing on stderr. After the first invocation,
the seeded cached file is absent. Repeating the command with no cached entries
also succeeds with the same output.

## Cleanup

Use the existing scenario's cleanup-safe lifetime. Remove this case's seeded
cache entries if they remain after a failure. Keep command recordings and the
isolated run directory as evidence. Do not touch the user's real cache or home.
