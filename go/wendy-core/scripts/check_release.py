"""Check the implemented C operation compatibility before a Core release.

This is not a complete ABI/wire/behavior checker. New ABI types and structured
messages require extending this checker; behavioral compatibility needs review.
"""
import json
import re
import subprocess

from build import ROOT, check_version
from versioning import TAG_PREFIX, RELEASE, base_version, identity, release_key


def require_compatible(before, after):
    old_version = release_key(before["version"])
    new_version = release_key(after["version"])
    if new_version <= old_version:
        raise ValueError("Core release versions must increase")
    old = {x["name"]: x["signature"] for x in before["c"]["functions"]}
    new = {x["name"]: x["signature"] for x in after["c"]["functions"]}
    breaking = any(new.get(name) != signature for name, signature in old.items())
    additions = set(new) - set(old)
    if breaking and new_version[0] <= old_version[0]:
        raise ValueError("A removed/changed C operation requires a major Core version bump")
    if additions and new_version[:2] <= old_version[:2]:
        raise ValueError("A new C operation requires a minor or major Core version bump")


def main():
    check_version()
    value = base_version()
    current = json.loads((ROOT / f"documentation/versions/{value}.json").read_text())
    tags = subprocess.check_output(["git", "tag", "--merged", "HEAD"], cwd=ROOT, text=True).splitlines()
    prior = []
    for tag in tags:
        if not re.fullmatch(re.escape(TAG_PREFIX) + RELEASE, tag):
            continue
        if identity()["channel"] == "stable" and tag == TAG_PREFIX + value:
            continue
        parts = release_key(tag[len(TAG_PREFIX):])
        prior.append((parts, tag))
    if not prior:
        print("Initial Core release: no previous version to compare")
        return
    _, tag = max(prior)
    raw = subprocess.check_output(["git", "show", f"{tag}:go/wendy-core/documentation/versions/{tag[len(TAG_PREFIX):]}.json"], cwd=ROOT)
    require_compatible(json.loads(raw), current)
    print(f"Implemented operation compatibility checked against {tag}")


if __name__ == "__main__":
    main()
