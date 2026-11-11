#!/usr/bin/env python3
"""Use Canonical HTTPS archives on disposable Ubuntu 24.04 CI runners."""

import re
from pathlib import Path


def configure(sources: Path) -> None:
    original = sources.read_text()
    result = []
    seen = set()
    ubuntu_uris = {
        "mirror+file:/etc/apt/apt-mirrors.txt",
        "http://azure.archive.ubuntu.com/ubuntu",
        "http://archive.ubuntu.com/ubuntu",
        "https://archive.ubuntu.com/ubuntu",
        "http://security.ubuntu.com/ubuntu",
        "https://security.ubuntu.com/ubuntu",
    }
    for stanza in re.split(r"\n[ \t]*\n", original.strip()):
        entries = re.findall(r"(?m)^([\w-]+):[ \t]*(.*)$", stanza)
        fields = {name.lower(): value for name, value in entries}
        uris = fields.get("uris", "").split()
        keyring = "/usr/share/keyrings/ubuntu-archive-keyring.gpg"
        if fields.get("signed-by") != keyring and not any(
            uri.rstrip("/") in ubuntu_uris for uri in uris
        ):
            result.append(stanza)
            continue
        # Fail before writing if the hosted image changes its Ubuntu layout.
        if (
            len(uris) != 1
            or uris[0].rstrip("/") not in ubuntu_uris
            or len(entries) != len(fields)
            or re.search(r"(?m)^[ \t]+\S", stanza)
            or fields.get("signed-by") != keyring
            or fields.get("types") != "deb"
        ):
            raise ValueError("Unexpected Ubuntu Deb822 source; refusing to rewrite")
        suites = fields.get("suites", "").split()
        if not suites or not set(suites) <= {
            "noble", "noble-updates", "noble-backports", "noble-security"
        }:
            raise ValueError("Expected only Ubuntu 24.04 (noble) suites")
        seen.update(suites)
        # Azure's selector can serve all pockets in one stanza. Split security
        # out so each pocket uses its explicit Canonical endpoint.
        for security in (False, True):
            selected = [s for s in suites if (s == "noble-security") == security]
            if not selected:
                continue
            host = "security" if security else "archive"
            updated = re.sub(r"(?im)^URIs:.*$", f"URIs: https://{host}.ubuntu.com/ubuntu/", stanza)
            updated = re.sub(r"(?im)^Suites:.*$", "Suites: " + " ".join(selected), updated)
            result.append(updated)
    if not {"noble", "noble-updates", "noble-security"} <= seen:
        raise ValueError("Missing expected Ubuntu 24.04 archive/security sources")
    sources.write_text("\n\n".join(result) + "\n")


if __name__ == "__main__":
    configure(Path("/etc/apt/sources.list.d/ubuntu.sources"))
