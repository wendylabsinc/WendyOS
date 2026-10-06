#!/usr/bin/env python3
"""Generate the CLI's end-user skill group from its single plugin source."""
import argparse
import json
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
SOURCE = ROOT / "plugins/wendy-agentic-coding/skills"
TARGET = ROOT / "go/internal/cli/assets/skills"
GROUP = SOURCE.parent / "skill-group.json"
MCP_TARGETS = [
    ROOT / "plugins/wendy-chatgpt/skills/wendy",
    ROOT / "plugins/wendy-get-started/skills/wendy",
    ROOT / "plugins/wendyos/skills/wendy",
    ROOT / "go/internal/cli/assets/mcp-skills/wendy",
]


def mcp_bundle(skills):
    """One importable skill, retaining all source files and relative links."""
    files = {}
    for name in skills:
        for source in (SOURCE / name).rglob("*"):
            if source.is_file():
                relative = source.relative_to(SOURCE / name)
                files[Path("references/skills") / name / relative] = source.read_bytes()
                if name == "wendy" and relative != Path("SKILL.md"):
                    files[relative] = source.read_bytes()
    main = (SOURCE / "wendy/SKILL.md").read_bytes()
    if not main.startswith(b"---\n") or b"\n---\n" not in main[4:]:
        raise SystemExit("The Wendy entrypoint needs YAML frontmatter")
    offset = main.index(b"\n---\n", 4) + len(b"\n---\n")
    index = """
## Bundled workflows

This package includes the complete Wendy end-user skill group. Read the
relevant workflow below before acting, and resolve its references relative to
that file. These supporting skills need no separate installation.

For first-time setup, read
[wendy-onboarding](references/skills/wendy-onboarding/SKILL.md). It guides the user
from their goal through CLI installation when needed, a local or hosted
connection, and their first physical device or simulator. It also works before
any Wendy MCP tools are connected. Do not assume the user knows Wendy or has
installed its CLI.

Inspect the available tools first. The CLI server uses `wendy_status` and
`device_list`; the ChatGPT gateway uses `list_robots` and `inspect_robot`.
Use the connected server's tools and explicit target identifiers. Use CLI
commands only when a local terminal is available. A skill cannot grant access
or add tools missing from the server.

"""
    index += "".join(f"- [{name}](references/skills/{name}/SKILL.md)\n" for name in skills)
    files[Path("SKILL.md")] = main[:offset] + index.encode() + b"\n" + main[offset:]
    return files


def sync_files(files, target, check, differences):
    for relative, content in files.items():
        destination = target / relative
        if not destination.exists() or destination.read_bytes() != content:
            differences.append(str(destination.relative_to(ROOT)))
            if not check:
                destination.parent.mkdir(parents=True, exist_ok=True)
                destination.write_bytes(content)
    extras = {p.relative_to(target) for p in target.rglob("*") if p.is_file()} - files.keys()
    if extras:
        raise SystemExit(f"Remove stale generated files in {target}: {sorted(map(str, extras))}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--check", action="store_true")
    args = parser.parse_args()
    differences = []
    group = json.loads(GROUP.read_text())
    skills = group["skills"]
    if group["name"] != SOURCE.parent.name or group["audience"] != "end-users":
        raise SystemExit("Expected the Wendy end-user skill group")
    actual = sorted(p.name for p in SOURCE.iterdir() if p.is_dir())
    if skills != actual:
        raise SystemExit("skill-group.json must list exactly the end-user plugin skills, sorted")

    manifest_target = TARGET / "end-user-group.json"
    if not manifest_target.exists() or manifest_target.read_bytes() != GROUP.read_bytes():
        differences.append(str(manifest_target.relative_to(ROOT)))
        if not args.check:
            manifest_target.write_bytes(GROUP.read_bytes())
    for skill in skills:
        files = {p.relative_to(SOURCE / skill): p for p in (SOURCE / skill).rglob("*") if p.is_file()}
        if Path("SKILL.md") not in files:
            raise SystemExit(f"Missing source skill: {skill}")
        for relative, source in files.items():
            target = TARGET / skill / relative
            if not target.exists() or target.read_bytes() != source.read_bytes():
                differences.append(str(target.relative_to(ROOT)))
                if not args.check:
                    target.parent.mkdir(parents=True, exist_ok=True)
                    target.write_bytes(source.read_bytes())
        extras = {p.relative_to(TARGET / skill) for p in (TARGET / skill).rglob("*") if p.is_file()} - files.keys()
        if extras:
            raise SystemExit(f"Remove stale embedded files for {skill}: {sorted(map(str, extras))}")
    bundle = mcp_bundle(skills)
    for target in MCP_TARGETS:
        sync_files(bundle, target, args.check, differences)
    plugin_source = ROOT / "plugins/wendy-chatgpt"
    plugin_files = {Path("plugin.json"): (plugin_source / "plugin.json").read_bytes()}
    plugin_files.update({
        p.relative_to(plugin_source): p.read_bytes()
        for p in (plugin_source / "assets").rglob("*") if p.is_file()
    })
    sync_files(
        plugin_files,
        ROOT / "go/internal/cli/assets/chatgpt-plugin", args.check, differences,
    )
    if args.check and differences:
        raise SystemExit("Run python3 scripts/sync-agent-skills.py:\n" + "\n".join(differences))
    print(f"{'Checked' if args.check else 'Synced'} {len(skills)} end-user skills")


if __name__ == "__main__":
    main()
