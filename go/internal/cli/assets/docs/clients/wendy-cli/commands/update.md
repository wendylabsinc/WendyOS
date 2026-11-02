# `wendy update`

Keep your Wendy CLI current without guessing which installer to use.

```sh
wendy update
```

Wendy identifies the running CLI's installation and checks the latest stable
release for stable installations. Nightly updates use their package manager
without a stable-release lookup. Wendy shows the update commands before asking
for confirmation. The prompt defaults to Yes. To approve an update in a script
or AI terminal:

```sh
wendy update --yes
```

Without an interactive terminal, `--yes` is required before any package manager
runs. JSON output also requires `--yes` when updating.

## Check without installing

```sh
wendy update --check
wendy update --check --json
```

`--check` makes a fresh stable-release request, including for nightly installations.
It does not install software, write the update cache, refresh authentication, or
emit update telemetry. If the release service cannot be reached, the command
fails rather than reporting that your CLI is current.

The JSON report contains `current_version`, `latest_stable_version`, `status`,
`update_available`, and `installation`. The installation includes its method,
channel, executable path, supported update commands, and any manual instructions.
These local paths are printed for troubleshooting, not included in update
telemetry. An available update is a successful check and exits with code 0.

## Installation methods

| Installation | What happens |
| --- | --- |
| Homebrew stable | Runs `brew update`, then `brew upgrade wendylabsinc/tap/wendy`. |
| Homebrew nightly | Keeps the nightly channel and upgrades `wendylabsinc/tap/wendy-nightly`. The stable release check does not establish whether nightly is current. |
| Recognized WinGet package | Runs `winget upgrade --id WendyLabs.Wendy --exact --source winget`. |
| Debian / Ubuntu package | Refreshes APT and upgrades the exact owning package with `apt-get install --only-upgrade`. |
| RPM package | Upgrades the exact owning package with DNF or YUM. |
| Arch / AUR package | Prints the owning package's upgrade instructions. |
| Manually copied binary or unrecognized installation | Prints instructions to update that installation explicitly. |
| Development build | Leaves the source-built binary alone. |

APT and RPM updates use sudo when needed. In CI or without a terminal, sudo runs
without a password prompt; arrange elevation first or follow the manual
instructions. Package confirmations are approved only after the Wendy confirmation
or explicit `--yes`. A package installed from a downloaded file may need a newer
file if no repository provides it.

For a manual update, complete the printed instructions,
then run `wendy update --check` again. An unsupported automatic update exits with
an error and instructions; it does not claim that an update happened.

## Verify the result

After an automatic update, Wendy runs the newly PATH-resolved `wendy --version`.
It only reports success when that executable is newer than the running CLI and
has reached the expected stable version, when one was supplied. A failed package
command, pinned package, or old executable still taking precedence on PATH is
reported as a failure.

If a second Wendy binary shadows the running CLI, correct PATH first. Use
`type -a wendy` on macOS/Linux or `Get-Command wendy -All` in PowerShell to find
copies. A package repository can also lag behind the published release; retry
after the package is available.

Restart the Wendy MCP server or your AI app after updating. An existing server
process continues using its old binary and tools until restarted. Rerun
`wendy mcp setup` if you also want to refresh the bundled assistant skills.

## Automatic reminders

Ordinary commands check for releases in the background. A successful check is
cached for 24 hours; a failed or cancelled check can retry after one hour. The
reminder waits at most one second for a background release request, with separate
bounded installation and cache lookups, so an unavailable release service does
not hold up your command.

After a successful command in an interactive terminal, a bold popup shows the
installed and available versions for supported stable installations. Choose
**Download now** to install with the detected package manager, or **Ask again
later...** to snooze all update prompts for 24 hours, even if another release
arrives. Use the arrow keys or Tab to choose, then Enter to confirm. Closing the
popup with Escape or Ctrl+C does not snooze it. Unsupported installations receive
manual update instructions. JSON output and automated invocations never open a
prompt. A command that failed can show a short, noninteractive
reminder while preserving its original error and exit status. An update failure
after a successful command also preserves the original command's success.

Local MCP servers also check for releases daily while running. Tool responses
include a prominent update notice once per release per session, repeated after
24 hours if the server is still outdated. The notice asks the assistant to tell
you about the update and includes the update command and server restart step.
`wendy_status` retains the update details between reminders.

CI, read-only checks, and completion helpers skip background reminders. Set
`WENDY_NO_UPDATE_CHECK=1` to disable background checks and notices; an explicit
`wendy update` or `wendy update --check` still works.

These behaviors become available after installing a release that includes them.
If your older CLI does not recognize `update`, upgrade it once using its existing
package manager or original installation method, then verify `wendy --version`.
