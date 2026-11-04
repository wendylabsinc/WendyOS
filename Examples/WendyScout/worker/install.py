import hashlib, json, pathlib, subprocess, tarfile

p = json.loads(pathlib.Path("pins.json").read_text())["pi"]
archive = pathlib.Path("/tmp/pi.tar.gz")
subprocess.run(
    [
        "curl",
        "-fL",
        "--retry",
        "2",
        "--connect-timeout",
        "15",
        "--max-time",
        "180",
        "-o",
        str(archive),
        p["url"],
    ],
    check=True,
    timeout=570,
)
if hashlib.sha256(archive.read_bytes()).hexdigest() != p["sha256"]:
    raise RuntimeError("Pi ARM64 digest mismatch")
dest = pathlib.Path("/opt/pi")
dest.mkdir()
with tarfile.open(archive) as t:
    t.extractall(dest, filter="data")
binary = next(p for p in dest.rglob("pi") if p.is_file())
pathlib.Path("/usr/local/bin/pi").symlink_to(binary)
subprocess.run([str(binary), "--version"], check=True)
archive.unlink()
