#!/usr/bin/env python3
"""Fingerprint Go source/assets, compiler and architecture for the Xcode build."""
import hashlib
import os
import pathlib
import platform
import subprocess
root = pathlib.Path(__file__).resolve().parents[3]
h = hashlib.sha256()
h.update(subprocess.check_output(["go", "version"]))
h.update(platform.machine().encode())
for base, dirs, files in os.walk(root / "go"):
    dirs[:] = sorted(d for d in dirs if d not in {".git", "Build", "build", "bin", "DerivedData", "node_modules"} and not d.endswith(".xcodeproj"))
    for name in sorted(files):
        p = pathlib.Path(base) / name
        if ("/macos/Sources/" in str(p) and p.suffix != ".h") or "/macos/Tests/" in str(p):
            continue
        # Stat captures embedded assets as well as Go and C inputs without
        # hashing large videos and simulator assets on every Xcode invocation.
        s = p.stat()
        h.update(f"{p.relative_to(root)}:{s.st_size}:{s.st_mtime_ns}".encode())
for name in ["go.mod", "go.sum"]:
    h.update((root / name).read_bytes())
print(h.hexdigest())
