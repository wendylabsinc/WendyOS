# Cache clear removes read-only descendants without changing external targets

## Setup

Use a fresh isolated CLI home, temporary directory and working directory. No
authentication or running agent is needed. In the CLI's platform-specific Wendy
cache directory, create an `images` directory containing `entry.bin` with the
text `cached` followed by a newline.

On POSIX platforms (macOS, Linux and WendyOS), also create
`simulator/g1_sim/simulation.py` inside the cache with exactly `cached` followed
by a newline. Set that file's permissions to 0444 and its containing `g1_sim`
directory's permissions to 0555. In the isolated working directory, outside the
cache, create `external-cache-target/keep` with exactly `keep` followed by a
newline. Set this file to 0444 and its containing directory to 0555. Create a
symlink named `external-link` inside the cache pointing to the absolute path of
`external-cache-target`. Verify the fixture's modes and that the symlink exists
before invoking the CLI; fixture setup failure must fail the test loudly.

The added POSIX permission and symlink requirements are scoped to POSIX. Windows
must retain the baseline writable-file and repeat-clear checks, with no whole-test
skip. This experiment makes no Windows permission or symlink behavior claim.

## Action

Run the real CLI command `wendy cache clear` without an interactive terminal.
Then run it a second time after checking the first result.

## Expectations

Each invocation exits successfully, prints exactly `Cache cleared.` followed by
a newline on stdout, and prints nothing on stderr. After the first invocation,
the seeded cached file is absent. On POSIX the entire cache directory, including
the read-only descendant and symlink, is absent. The external target directory
and file still exist, the file's bytes remain exactly `keep` followed by a
newline, and the directory and file modes remain exactly 0555 and 0444. Check
these external-target invariants after each invocation. Repeating the command
with no cached entries also succeeds with the same output. A success message
alone is insufficient evidence of deletion or of symlink safety.

## Cleanup

Use the existing scenario's cleanup-safe lifetime. Remove this case's seeded
cache entries if they remain after a failure. On POSIX, restore owner write/search
permission on only the known seeded read-only directory before removing any
leftover cache. Separately restore the case-owned external directory's owner
write/search permission and remove that fixture. Do not follow cached symlinks
while restoring permissions. Cleanup failures must fail loudly. Keep command
recordings and the isolated run directory as evidence. Do not touch the user's
real cache or home.
