# Wendy skill groups

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
