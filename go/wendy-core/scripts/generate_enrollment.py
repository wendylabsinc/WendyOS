"""Generate private enrollment messages from the unchanged monorepo schemas."""
import os
from pathlib import Path
import re
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parents[1]
PROTO_ROOT = ROOT.parents[1] / "Proto"
MODULE = "github.com/wendylabsinc/WendyOS/go/wendy-core"
SOURCES = (
    "wendycloud/v2/device_enrollment.proto",
    "wendycloud/v2/assets.proto",
    "wendycloud/v2/pki_admin.proto",
    "wendy/agent/services/v2/provisioning_service.proto",
    "wendy/agent/services/v2/device_info_service.proto",
)

def group(source):
    return {"wendycloud": "cloudpb", "pkicore": "pkipb", "wendyauth": "authpb"}.get(source.split("/")[0], "agentpb" if "/agent/" in source else "optionspb")

def main():
    actual = subprocess.check_output(["protoc", "--version"], text=True).strip()
    if actual != "libprotoc 36.2":
        raise SystemExit(f"Expected libprotoc 36.2, got {actual}")
    sources = set()
    def visit(source):
        if source.startswith("google/") or source in sources:
            return
        sources.add(source)
        for dependency in re.findall(r'import\s+"([^\"]+)"', (PROTO_ROOT / source).read_text()):
            visit(dependency)
    for source in SOURCES:
        visit(source)
    with tempfile.TemporaryDirectory(prefix="wendy-enrollment-proto-") as directory:
        staging = Path(directory)
        env = {**os.environ, "GOBIN": str(staging), "GOPROXY": "https://proxy.golang.org"}
        subprocess.run(["go", "install", "google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.10"], cwd=ROOT, env=env, check=True)
        plugin = staging / ("protoc-gen-go.exe" if os.name == "nt" else "protoc-gen-go")
        subprocess.run(["protoc", "-I", str(PROTO_ROOT), "--plugin=protoc-gen-go=" + str(plugin),
                        "--go_out=" + str(staging), "--go_opt=module=" + MODULE,
                        *("--go_opt=M" + s + "=" + MODULE + "/core/enrollment/internal/" + group(s) for s in sorted(sources)),
                        *sorted(sources)], cwd=ROOT, check=True)
        for path in (staging / "core/enrollment/internal").rglob("*.pb.go"):
            text = path.read_text()
            text = text.replace('import (', 'import (\n registry "' + MODULE + '/core/enrollment/internal/registry"')
            fm, tm = "File: protoimpl.DescBuilder{\n", "out := protoimpl.TypeBuilder{\n"
            if text.count(fm) != 1 or text.count(tm) != 1:
                raise SystemExit(f"Unexpected generator shape: {path.name}")
            text = text.replace(fm, fm + "\t\t\tFileRegistry: registry.Files,\n\t\t\tTypeResolver: registry.Types,\n")
            text = text.replace(tm, tm + "\t\tTypeRegistry: registry.Types,\n")
            dest = ROOT / path.relative_to(staging)
            dest.parent.mkdir(parents=True, exist_ok=True)
            dest.write_text(text)
        subprocess.run(["gofmt", "-w", str(ROOT / "core/enrollment/internal")], check=True)

if __name__ == "__main__":
    main()
