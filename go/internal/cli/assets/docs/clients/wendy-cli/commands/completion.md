# `wendy completion`

Generates or installs shell completion scripts for the `wendy` CLI.

> **Note:** `wendy completion` is not listed in `wendy --help` to keep the
> top-level surface focused, but it is fully functional. Run
> `wendy completion --help` directly to see its subcommands.

## Usage

```sh
wendy completion <shell>
wendy completion install [flags]
```

## Subcommands

| Subcommand | Description |
|---|---|
| `bash` | Print the bash completion script to stdout |
| `zsh` | Print the zsh completion script to stdout |
| `fish` | Print the fish completion script to stdout |
| `powershell` | Print the PowerShell completion script to stdout |
| `install` | Auto-detect the current shell and install the script to the conventional location |

## Printing a script

Each shell subcommand writes the completion script to stdout. You can source it directly or redirect it to a file:

```sh
wendy completion bash
wendy completion zsh
wendy completion fish
wendy completion powershell
```

## `wendy completion install`

Detects the running shell from `$SHELL` (Unix) or defaults to `powershell` (Windows) and writes the completion script to the conventional location for that shell, appending an idempotent sourcing block to the shell rc file when required.

Running `install` more than once is safe — the rc file is only modified on the first run. Subsequent runs detect the `# wendy-completion` sentinel and make no changes.

A successful `install` also records `completionInstalled: true` in `~/.wendy/config.json`, which permanently suppresses the [ambient install prompt](#automatic-prompt-to-install-completions). The dry-run flags `--print-path` and `--stdout` do **not** set this flag, since they don't actually install anything.

### Shell detection and install paths

| Shell | Script path | rc file modified |
|---|---|---|
| bash (bash-completion v2 detected) | `${XDG_DATA_HOME:-~/.local/share}/bash-completion/completions/wendy` | No |
| bash (fallback) | `~/.wendy/completions/wendy.bash` | `~/.bashrc` |
| zsh | `~/.zfunc/_wendy` | `${ZDOTDIR:-~}/.zshrc` |
| fish | `${XDG_CONFIG_HOME:-~/.config}/fish/completions/wendy.fish` | No (fish auto-loads) |
| powershell (Unix) | `~/.config/powershell/Completions/wendy.ps1` | `~/.config/powershell/Microsoft.PowerShell_profile.ps1` |
| powershell (Windows) | `~/Documents/PowerShell/Completions/wendy.ps1` | `~/Documents/PowerShell/Microsoft.PowerShell_profile.ps1` |

bash-completion v2 is detected by probing standard locations (`${XDG_DATA_HOME}/bash-completion`, `/etc/bash_completion`, `/usr/local/etc/bash_completion`, `/opt/homebrew/etc/bash_completion`). When found the script is written to the XDG path and no rc edit is needed. When not found, a stand-alone script is written to `~/.wendy/completions/wendy.bash` and a sourcing line is appended to `~/.bashrc`.

### Flags

| Flag | Description |
|---|---|
| `--shell bash\|zsh\|fish\|powershell` | Override shell auto-detection |
| `--print-path` | Dry run — print the computed script and rc paths, then exit without writing anything |
| `--stdout` | Print the completion script to stdout without writing any files or modifying rc files |

`--stdout` and `--print-path` are mutually exclusive; combining them returns an error.

### Examples

Install completions for the detected shell:

```sh
wendy completion install
```

Install completions for a specific shell:

```sh
wendy completion install --shell zsh
```

Check where files would be written without writing them:

```sh
wendy completion install --shell fish --print-path
```

Print the completion script to stdout (useful for Homebrew formula staging):

```sh
wendy completion install --shell bash --stdout
```

The `--stdout` flag is intended for package managers such as Homebrew that call the binary directly and expect the completion script on stdout (e.g. via `generate_completions_from_executable`). It prints the script for the selected shell and exits without touching the filesystem or shell rc files.

After installation, restart your shell (or source the relevant rc file) for completions to take effect. Fish and bash-completion v2 load the script automatically on the next shell start without any rc change.

## Automatic prompt to install completions

When shell completions aren't installed yet, the CLI may offer to install them with an ambient prompt after a command finishes:

```
Shell completions for `wendy` aren't installed yet.
Install them now? [y/n]
```

The prompt has no default — you must answer `y` or `n`.

- **`y`** installs completions for the detected shell (same as `wendy completion install`).
- **`n`** permanently dismisses the prompt (sets `completionPromptDismissed`); it won't be shown again.
- **Ctrl+C / EOF** leaves the prompt unanswered. It is throttled and won't reappear until the throttle window (24 hours) elapses.

The prompt is intentionally unobtrusive and is **never** shown when:

- the session is non-interactive (no TTY) or output is machine-readable (`--json`),
- completions are already installed (`completionInstalled`) or the prompt was dismissed (`completionPromptDismissed`),
- the first-run analytics notice or a CLI-update prompt is shown in the same invocation (the prompt never stacks on top of those),
- the command runs its own completion flow — `wendy completion …` and [`wendy tour`](./tour.md) — or is an internal helper.

> **See also:** [`wendy tour`](./tour.md) includes a completions step that installs shell completions as part of first-time setup. Completing the tour, or installing completions there, suppresses this ambient prompt.

### Config fields

The prompt's state is persisted in `~/.wendy/config.json`:

| Field | Type | Meaning |
|---|---|---|
| `completionInstalled` | bool | Set once `wendy completion install` (or the prompt/tour install path) succeeds. Permanently suppresses the prompt. |
| `completionPromptDismissed` | bool | Set when you answer `n` to the prompt. Permanently suppresses the prompt. |
| `lastCompletionPromptCheck` | RFC3339 timestamp | When the prompt was last shown. Used to throttle it to at most once per 24-hour window. |
