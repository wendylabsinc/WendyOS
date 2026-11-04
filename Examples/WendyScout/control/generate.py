"""Generate clients from pinned canonical repository schemas, never a parallel schema."""

import hashlib, json, pathlib, subprocess, urllib.request

root = pathlib.Path("canonical")
m = json.loads(pathlib.Path("canonical.json").read_text())
paths = []
for source, digest in m["files"].items():
    url = f"https://raw.githubusercontent.com/wendylabsinc/WendyOS/{m['revision']}/{source}"
    with urllib.request.urlopen(url, timeout=30) as r:
        data = r.read(512 * 1024)
    if hashlib.sha256(data).hexdigest() != digest:
        raise RuntimeError("Canonical schema digest mismatch")
    dest = root / source.removeprefix("Proto/")
    dest.parent.mkdir(parents=True, exist_ok=True)
    dest.write_bytes(data)
    paths.append(str(dest))
subprocess.run(["protoc", "-I" + str(root), "--python_out=.", *paths], check=True)
