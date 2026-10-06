# Wendy skill groups

For users who do not yet know Wendy or have its CLI, the skills-only
[getting-started plugin](wendy-get-started/README.md) guides them through choosing
local hardware, a simulator, or an existing hosted connection. It can be used
before any Wendy MCP server is connected. Hosted use needs no local CLI.

`wendy mcp setup chatgpt` creates the local ChatGPT package and personal
marketplace with a restricted first-run policy. `--connection both --app-id
<registered-MCP-app-id>` adds a separate hosted package bound to an existing
registered connection. The broader developer server is opt-in through
`--developer-tools`. See the [ChatGPT setup instructions](wendy-chatgpt/README.md).

The separate [Wendy ChatGPT robot plugin](wendy-chatgpt/README.md) provides a
robot panel, scoped device operations, and reviewed app tools. Its gateway is
configured separately from the agentic coding skill groups below.

`wendy-agentic-coding` is the single end-user group. It covers CLI and MCP setup,
device installation and first boot, app creation, deployment, debugging, and
robot verification. Its `skills/` directory is the source for both the plugin
and the CLI's bundled skills. Edit skills there and run:

```sh
python3 scripts/sync-agent-skills.py
python3 scripts/sync-agent-skills.py --check
```

`skill-group.json` lists the complete group. Setup installs only those entries,
including their references, regardless of what other skills the CLI embeds.
CI checks the manifest against the plugin directory and generated CLI copies.

The sync script also builds one `wendy` bundle for the ChatGPT plugin and MCP
server. It includes the whole end-user group under `references/skills/`, with
relative links intact. This keeps the catalog within OpenAI's five-skill import
limit. Edit the original skills here, then sync; do not edit generated copies.

`wendy-onboarding` helps new users install and verify their first physical device,
or create and boot a simulator while waiting for hardware. The Codex and ChatGPT
manifests select it through `extensions.com.openai.onboardingSkill`.

Both `wendy mcp serve` and the ChatGPT gateway advertise the skills extension.
Call `skills/list` with `{}`, `skills/get` with the returned `uri`, and
`resources/read` for each listed file. The catalog uses one terminal page and
SHA-256 resource digests. Skill reads need no device connection. Gateway HTTP
requests still require authentication. See the
[OpenAI MCP skill import contract](https://developers.openai.com/plugins/build/mcp-server#import-skills-from-the-mcp-server).

After upgrading the CLI, restart the MCP process. For an OpenAI submission,
run Scan Tools again and review the imported snapshot before publishing the
next plugin version; the snapshot does not update with the running server.

`wendy mcp setup` installs this group for detected tools:

| Assistant | Skill location |
| --- | --- |
| Codex | `~/.agents/skills/<name>/SKILL.md` |
| Claude Code | `~/.claude/skills/<name>/SKILL.md` |
| OpenCode | `~/.agents/skills/<name>/SKILL.md`, shared with Codex |

`wendy init --assistant claude --install-claude-skills` uses the same group.
The installer preserves pre-existing or edited skill files and reports conflicts.
It does not manage Claude's private plugin registry. The optional
`wendy-agentic-coding` plugin packages these same skills for plugin installation.
Choose CLI-managed skills or the plugin to avoid duplicate skill entries.

`wendy-engineering` is a separate, opt-in group for changing Wendy itself. It
holds repository orientation, PR workflows, and implementation debugging. CLI
setup never installs it. The separate `claude-skills` repository is not a source
or installation dependency for the end-user group.

[`wendy-mods`](wendy-mods/README.md) is an opt-in Claude Code mod, a plugin of
function hooks rather than skills. It verifies Wendy MCP deploys and shows the
connected device above the prompt. It is not in any marketplace or the public
mirror while the mod API is early access.

## Existing installations

Setup does not uninstall separately installed plugins or personal engineering
skills. If an earlier Wendy CLI installed per-skill `@wendy-skills` plugins, or
you installed the old `claude-skills` marketplace, review those entries in
Claude's `/plugin` menu and disable the ones you no longer want. Keep intentional
engineering installations. Older loose `wendy-skills.md` files are no longer
generated or updated by setup.

These native skill paths follow the documented discovery rules for
[Codex](https://learn.chatgpt.com/docs/build-skills),
[Claude Code](https://code.claude.com/docs/en/skills), and
[OpenCode](https://opencode.ai/docs/skills/).
