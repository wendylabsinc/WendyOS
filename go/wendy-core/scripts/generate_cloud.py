"""Generate Core's private Cloud messages from the monorepo contract."""
import os
from pathlib import Path
import shutil
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parents[1]
PROTO_ROOT = ROOT.parents[1] / "Proto"
OUTPUT = ROOT / "core/cloud/internal/cloudpb"
MODULE = "github.com/wendylabsinc/WendyOS/go/wendy-core"
PACKAGE = MODULE + "/core/cloud/internal/cloudpb"
PROTOC_VERSION = "libprotoc 36.2"
GO_GENERATOR_VERSION = "v1.36.10"
SOURCES = (
    "wendy/options.proto",
    "wendycloud/v2/signed_request.proto",
    "wendycloud/v2/organizations.proto",
    "wendycloud/v2/assets.proto",
)


def main():
    actual = subprocess.check_output(["protoc", "--version"], text=True).strip()
    if actual != PROTOC_VERSION:
        raise SystemExit(f"Expected {PROTOC_VERSION}, got {actual}")
    with tempfile.TemporaryDirectory(prefix="wendy-cloud-proto-") as directory:
        staging = Path(directory)
        environment = {**os.environ, "GOBIN": str(staging), "GOPROXY": "https://proxy.golang.org"}
        subprocess.run(
            ["go", "install", "google.golang.org/protobuf/cmd/protoc-gen-go@" + GO_GENERATOR_VERSION],
            cwd=ROOT, env=environment, check=True,
        )
        plugin = staging / ("protoc-gen-go.exe" if os.name == "nt" else "protoc-gen-go")
        subprocess.run(
            ["protoc", "-I", str(PROTO_ROOT), "--plugin=protoc-gen-go=" + str(plugin),
             "--go_out=" + str(staging), "--go_opt=module=" + MODULE,
             *("--go_opt=M" + source + "=" + PACKAGE for source in SOURCES), *SOURCES],
            cwd=ROOT, check=True,
        )
        generated = staging / "core/cloud/internal/cloudpb"
        for source in SOURCES:
            path = generated / (Path(source).stem + ".pb.go")
            text = path.read_text()
            # Keep protobuf names and descriptors unchanged. Only redirect the
            # generated builders' registration targets: the CLI also links its
            # own generated copies of these exact service messages.
            file_marker = "File: protoimpl.DescBuilder{\n"
            type_marker = "out := protoimpl.TypeBuilder{\n"
            if text.count(file_marker) != 1 or text.count(type_marker) != 1:
                raise SystemExit(f"Unexpected protoc-gen-go builder shape in {path.name}")
            text = text.replace(file_marker, file_marker + "\t\t\tFileRegistry: privateFiles,\n\t\t\tTypeResolver: privateTypes,\n")
            text = text.replace(type_marker, type_marker + "\t\tTypeRegistry: privateTypes,\n")
            path.write_text(text)
        subprocess.run(["gofmt", "-w", str(generated)], check=True)
        OUTPUT.mkdir(parents=True, exist_ok=True)
        for path in generated.glob("*.pb.go"):
            shutil.copyfile(path, OUTPUT / path.name)


if __name__ == "__main__":
    main()
