#!/usr/bin/env python3
"""Export the local Wendy package without policies or credentials."""

import argparse
import json
from pathlib import Path
import struct
import zipfile


ROOT = Path(__file__).resolve().parents[1]
SOURCE = ROOT / "plugins/wendy-chatgpt"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, required=True, help="Destination ZIP outside the plugin directory")
    args = parser.parse_args()
    output = args.output.resolve()
    if output.is_relative_to(SOURCE.resolve()):
        parser.error("Write the ZIP outside the source plugin directory")

    manifest = json.loads((SOURCE / "plugin.json").read_text())
    extension = manifest["extensions"]["com.openai"]
    interface = extension["interface"]
    assert manifest["name"] == "wendy-robots"
    assert not manifest.get("apps") and not extension.get("apps")
    assert not extension.get("hooks")
    assert len(interface["displayName"]) <= 30
    assert len(interface["shortDescription"]) <= 30
    assert len(interface["longDescription"]) <= 4000
    prompts = interface["defaultPrompt"]
    assert isinstance(prompts, list) and 1 <= len(prompts) <= 3
    assert all(isinstance(p, str) and p.strip() and len(p) <= 128 and "\n" not in p for p in prompts)
    assert len({" ".join(p.split()) for p in prompts}) == len(prompts)

    mcp = json.loads((SOURCE / "mcp.json").read_text())
    assert mcp["mcpServers"] == {"wendy-robots": {"type": "stdio", "command": "wendy", "args": ["mcp", "gateway"]}}

    # Only portable components belong in the archive. Never export a user's
    # generated absolute-path MCP configuration, gateway policy or CLI login.
    files = [SOURCE / "plugin.json", SOURCE / "mcp.json"]
    for directory in (SOURCE / "skills", SOURCE / "assets"):
        for path in sorted(directory.rglob("*")):
            if path.is_symlink():
                parser.error(f"Symlinks are not allowed: {path}")
            if path.is_file():
                files.append(path)
    assert SOURCE / "skills/wendy/SKILL.md" in files
    for key, minimum in (("logo", 256), ("composerIcon", 48)):
        relative = interface[key]
        assert relative.startswith("./assets/")
        path = (SOURCE / relative).resolve()
        assert path.is_relative_to(SOURCE.resolve()) and path in files
        data = path.read_bytes()
        assert data[:8] == b"\x89PNG\r\n\x1a\n" and len(data) <= 5 * 1024 * 1024
        width, height = struct.unpack(">II", data[16:24])
        assert width == height and width >= minimum

    output.parent.mkdir(parents=True, exist_ok=True)
    with zipfile.ZipFile(output, "w", zipfile.ZIP_DEFLATED) as archive:
        for path in files:
            assert not path.is_symlink()
            name = str(Path(manifest["name"]) / path.relative_to(SOURCE))
            info = zipfile.ZipInfo(name, date_time=(2026, 1, 1, 0, 0, 0))
            info.compress_type = zipfile.ZIP_DEFLATED
            info.external_attr = 0o100644 << 16
            archive.writestr(info, path.read_bytes())
    with zipfile.ZipFile(output) as archive:
        assert archive.testzip() is None
        prefix = manifest["name"] + "/"
        assert json.loads(archive.read(prefix + "plugin.json")) == manifest
        assert json.loads(archive.read(prefix + "mcp.json")) == mcp
        for key in ("logo", "composerIcon"):
            assert prefix + interface[key].removeprefix("./") in archive.namelist()

    print(f"Local package: {output} ({output.stat().st_size:,} bytes; {len(files)} files)")
    print("Public submission readiness: incomplete. Local MCP acceptance must be confirmed with OpenAI.")
    missing = [k for k in ("websiteURL", "supportURL", "privacyPolicyURL", "termsOfServiceURL") if not interface.get(k)]
    if missing:
        print("Missing listing fields: " + ", ".join(missing))
    print("Also verify publisher identity, URL contents, country/commerce details, review cases and demo.")


if __name__ == "__main__":
    main()
