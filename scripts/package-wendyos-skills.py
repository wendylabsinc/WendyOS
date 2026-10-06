#!/usr/bin/env python3
"""Export and validate the skills-only Wendy public submission archive."""
import argparse
import json
from pathlib import Path
import re
import struct
import zipfile

ROOT = Path(__file__).resolve().parents[1]
SOURCE = ROOT / "plugins/wendyos"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    output = args.output.resolve()
    if output.is_relative_to(SOURCE.resolve()):
        parser.error("Write the ZIP outside the source plugin directory")
    manifest = json.loads((SOURCE / "plugin.json").read_text())
    assert manifest["name"] == "wendyos"
    assert re.fullmatch(r"\d+\.\d+\.\d+", manifest["version"])
    extension = manifest["extensions"]["com.openai"]
    interface = extension["interface"]
    assert not any(key in manifest for key in ("apps", "mcpServers", "skills"))
    assert not any(key in extension for key in ("apps", "hooks"))
    for key, limit in (("displayName", 30), ("shortDescription", 30),
                       ("longDescription", 4000), ("developerName", 80)):
        assert 0 < len(interface[key]) <= limit, key
    prompts = interface["defaultPrompt"]
    assert isinstance(prompts, list) and 1 <= len(prompts) <= 3
    assert all(isinstance(p, str) and p.strip() and len(p) <= 128 and "\n" not in p for p in prompts)
    assert len({" ".join(p.split()) for p in prompts}) == len(prompts)
    assert extension["publication"]["release_notes"].strip()
    assert extension["publication"]["countries"] == []
    assert extension["review"]["commerce"] is False

    files = [SOURCE / "plugin.json"]
    for directory in (SOURCE / "skills", SOURCE / "assets"):
        for path in sorted(directory.rglob("*")):
            assert not path.is_symlink(), path
            if path.is_file():
                files.append(path)
    skills = list((SOURCE / "skills").glob("*/SKILL.md"))
    assert len(skills) == 1 and skills[0].parent.name == "wendy"
    for path in (SOURCE / "skills").rglob("SKILL.md"):
        text = path.read_text()
        assert text.startswith("---\n") and "\n---\n" in text[4:], path
        frontmatter = text[4:].split("\n---\n", 1)[0]
        assert f"name: {path.parent.name}\n" in frontmatter + "\n", path
        assert "description: " in frontmatter, path
    assert (SOURCE / extension["onboardingSkill"]).resolve() in files
    for key, minimum in (("logo", 256), ("composerIcon", 48)):
        relative = interface[key]
        assert relative.startswith("./assets/")
        path = (SOURCE / relative).resolve()
        assert path.is_relative_to(SOURCE.resolve()) and path in files
        data = path.read_bytes()
        assert data[:8] == b"\x89PNG\r\n\x1a\n" and len(data) <= 5 * 1024 * 1024
        width, height = struct.unpack(">II", data[16:24])
        assert width == height and minimum <= width <= 4096
    output.parent.mkdir(parents=True, exist_ok=True)
    with zipfile.ZipFile(output, "w", zipfile.ZIP_DEFLATED) as archive:
        for path in files:
            name = str(Path(manifest["name"]) / path.relative_to(SOURCE))
            info = zipfile.ZipInfo(name, date_time=(2026, 1, 1, 0, 0, 0))
            info.compress_type = zipfile.ZIP_DEFLATED
            info.external_attr = 0o100644 << 16
            archive.writestr(info, path.read_bytes())
    with zipfile.ZipFile(output) as archive:
        assert archive.testzip() is None
        assert json.loads(archive.read("wendyos/plugin.json")) == manifest
        assert set(archive.namelist()) == {str(Path("wendyos") / p.relative_to(SOURCE)) for p in files}
        assert not any(Path(p).name in ("mcp.json", ".app.json") for p in archive.namelist())
    required = ("websiteURL", "supportURL", "privacyPolicyURL", "termsOfServiceURL")
    missing = [key for key in required if not interface.get(key)]
    print(f"Skills-only archive: {output} ({output.stat().st_size:,} bytes; {len(files)} files)")
    print("Listing URL fields: " + ("complete" if not missing else "missing " + ", ".join(missing)))
    print("Publisher verification, attestations, portal validation, and public review remain separate steps.")


if __name__ == "__main__":
    main()
