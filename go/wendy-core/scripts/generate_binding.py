"""Generate the native binding messages from Core's public protobuf contract."""
import os
from pathlib import Path
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parents[1]
MODULE = "github.com/wendylabsinc/WendyOS/go/wendy-core"


def main():
    actual = subprocess.check_output(["protoc", "--version"], text=True).strip()
    if actual != "libprotoc 36.2":
        raise SystemExit(f"Expected libprotoc 36.2, got {actual}")
    with tempfile.TemporaryDirectory(prefix="wendy-binding-proto-") as directory:
        staging = Path(directory)
        subprocess.run(
            ["go", "install", "google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.10"],
            cwd=ROOT, env={**os.environ, "GOBIN": str(staging), "GOPROXY": "https://proxy.golang.org"}, check=True,
        )
        plugin = staging / ("protoc-gen-go.exe" if os.name == "nt" else "protoc-gen-go")
        subprocess.run(
            ["protoc", "-I", "proto", "--plugin=protoc-gen-go=" + str(plugin),
             "--go_out=.", "--go_opt=module=" + MODULE, "proto/wendy/core/v1/core.proto"],
            cwd=ROOT, check=True,
        )


if __name__ == "__main__":
    main()
