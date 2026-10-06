# wendy-mods

A Claude Code mod (a plugin of function hooks) for people building on WendyOS.

- **Deploy verifier.** When the Wendy MCP `run` tool reports a started deploy, the
  mod checks the app and adds the verdict to the `run` result, so the model cannot
  report success on a detached deploy alone. A follow-up check reports apps that
  crash, restart or never become ready. It runs 20 s later, or when the app's
  readiness window ends.
- **Device band.** A row above the prompt shows the connected device, its
  transport, and the last deploy's verdict in the CLI's colors.

The mod calls only `wendy_status`, `app_inspect` and `container_list`, all
read-only. It never connects, deploys, changes tool groups, or starts a turn by
itself. App log lines it quotes to the model are marked as untrusted app output.

## Requirements

- Claude Code 2.1.287 or newer. The function-hook API is early access and can change.
- Wendy CLI 2026.09.30 or newer, with its MCP server connected (`wendy mcp setup`,
  or the `wendy` plugin). Older CLIs get "not verified" with the reason.
- For readiness checks, enable the observability tools group
  (`wendy_tools(groups=["observability"])`).
  - Without it, `app_inspect` is not listed, and Claude Code lets a mod call only
    listed tools.
  - The mod then checks app state with `container_list`: crash loops, stops and
    restarts are still caught, and readiness is reported as unknown.

## Try it

```sh
claude --plugin-dir plugins/wendy-mods
```

If your Wendy MCP server has another name, set it under `/config` → wendy-mods →
"Wendy MCP server".

## Develop

```sh
cd plugins/wendy-mods
claude plugin validate .
claude plugin test .
npx -y -p typescript@5.9 tsc -p .   # after one load has written .claude-plugin/types
```

All decisions live in plain modules under `hooks/` that take no engine object.
`hooks/register.tsx` only wires engine events to them. `tests/live-fixtures.ts`
holds output recorded from a real device; re-record it rather than editing it.

## Limits

- **Claude Code terminal and desktop sessions only.** VS Code, Codex and ChatGPT do
  not run mods.
- **The band draws only in the session that loaded the mod.** The desktop app shows
  a terminal-started session through the session handoff without attaching to it,
  so the band does not appear there. A session the desktop app starts needs the
  mod loaded in that session (`CLAUDE_CODE_PLUGIN_DIRS` in the `env` block of
  `~/.claude/settings.json`).
- **`wendy run` typed in a terminal is not verified;** it streams logs itself.
- **A deploy to a device other than the connected one is reported as not verified.**
- **The band names the device by its address** (`wendy_status` reports no device name).
- **Not published anywhere yet.** It is in no marketplace, and is excluded from the
  public `wendy-agentic-coding` mirror, until the mod API settles.
