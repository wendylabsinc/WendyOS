#!/usr/bin/env python3
"""Generate the CLI's end-user skill group from its single plugin source."""
import argparse
import json
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
SOURCE = ROOT / "plugins/wendy-agentic-coding/skills"
TARGET = ROOT / "go/internal/cli/assets/skills"
GROUP = SOURCE.parent / "skill-group.json"


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
    if args.check and differences:
        raise SystemExit("Run python3 scripts/sync-agent-skills.py:\n" + "\n".join(differences))
    print(f"{'Checked' if args.check else 'Synced'} {len(skills)} end-user skills")


if __name__ == "__main__":
    main()
