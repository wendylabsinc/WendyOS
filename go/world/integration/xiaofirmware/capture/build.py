"""Build the PTY-only firmware observer from the integrated Wendy source tree.

The normal Wendy Lite client handles protocol negotiation. Build receipts and
binaries remain in a fresh directory outside the repository.
"""
import argparse
import hashlib
import json
from pathlib import Path
import subprocess


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--wendy-source", "--lite-source", dest="source", type=Path, required=True)
    parser.add_argument("--out", type=Path, required=True)
    args = parser.parse_args()
    source, out = args.source.resolve(), args.out.resolve()
    local = Path(__file__).resolve().parent
    if source != local.parents[4] or not (source / "go.mod").is_file():
        parser.error("select the Wendy checkout containing this observer")
    if out.is_relative_to(source) or out.exists():
        parser.error("use a fresh build directory outside the repository")
    out.mkdir(mode=0o700, parents=True)
    package = "./go/world/integration/xiaofirmware/capture"
    inputs = {}
    # Hash actual repository packages in the build dependency closure, including
    # uncommitted integration edits. External module versions stay in go.sum.
    listed = subprocess.check_output(["go", "list", "-deps", "-f", "{{.Dir}}", package], cwd=source, text=True)
    for entry in sorted(set(listed.splitlines())):
        directory = Path(entry).resolve()
        if directory.is_relative_to(source):
            for path in sorted(directory.iterdir()):
                if path.is_file() and path.suffix in {".go", ".c", ".h", ".m"}:
                    inputs[str(path.relative_to(source))] = hashlib.sha256(path.read_bytes()).hexdigest()
    for path in [source / "go.mod", source / "go.sum", local / "build.py"]:
        inputs[str(path.relative_to(source))] = hashlib.sha256(path.read_bytes()).hexdigest()
    with (out / "build.log").open("w") as log:
        for command in [["go", "test", package], ["go", "build", "-o", str(out / "capture"), package]]:
            subprocess.run(command, cwd=source, stdout=log, stderr=subprocess.STDOUT, check=True)
    receipt = {
        "client_commit": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=source, text=True).strip(),
        "client_worktree_modified": bool(subprocess.check_output(["git", "status", "--porcelain", "--untracked-files=normal"], cwd=source, text=True).strip()),
        "client_source_inputs": inputs,
        "go_version": subprocess.check_output(["go", "version"], cwd=source, text=True).strip(),
        "binary_sha256": hashlib.sha256((out / "capture").read_bytes()).hexdigest(),
        "transport": "normal Wendy Lite ConnectToSerial, restricted to local PTYs",
    }
    (out / "build-receipt.json").write_text(json.dumps(receipt, indent=2) + "\n")
    print(out / "capture")


if __name__ == "__main__":
    main()
