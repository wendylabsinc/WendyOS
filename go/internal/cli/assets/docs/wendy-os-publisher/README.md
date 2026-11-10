A tool that uploads, removes and promotes (nightly -> release) builds for [WendyOS](/docs/advanced/wendyos/requirements) and [Wendy Lite](/docs/advanced/wendy-lite).

https://github.com/wendylabsinc/wendy-os-publisher

Wendy OS Publisher is integrated into WendyOS CI on Github actions, where it uploads as nightly.
You can manually run this tool to promote a build to release.

## CI integration

### Nightly builds

The nightly pipeline runs all device builds in parallel. The `publish` job:

- Always updates the master manifest (runs even if some builds are cancelled).
- Creates a GitHub pre-release and sends a Discord announcement **only when all device builds succeed**. If any build fails, neither the pre-release nor the Discord notification fires.
- The GitHub pre-release is tagged `nightly` and also tagged `${VERSION}-nightly`. Release notes are auto-generated from PRs and commits since the last `vX.Y.Z` tag (`--generate-notes`).
- The Discord announcement is sent via a single `curl`/`jq` step in the `publish` job, not per-device.

### Release builds

The release pipeline triggers on `v*` tags. A dedicated `announce` job sends the Discord notification **only after all of the following jobs succeed**: `release`, `publish-linux-repos`, `publish-aur`, and `publish-winget`. If any of those jobs fails, the Discord announcement does not fire. The `announce` job also only runs for stable (non-prerelease) builds.

- A GitHub release is created by the `release` job (idempotent — skipped if the tag already has a release).
- The Discord announcement uses `jq` to build the webhook payload (preventing JSON injection) and validates that the webhook URL matches the Discord API domain before use. It retries up to three times on failure; if all attempts fail the release is still considered successful and the notification is silently skipped.
- The Discord webhook is invoked once from the `announce` job, not per-device.
