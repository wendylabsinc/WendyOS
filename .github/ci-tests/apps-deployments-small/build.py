"""Build separate small ARM64 OCI layouts and verify the actual local images.

Uses the installed Go toolchain, Docker engine and Buildx; no dependency or base
image download is needed. Does not contact Wendy Cloud or any lab device.
"""

import argparse
import hashlib
import json
import os
from pathlib import Path
import socket
import subprocess
import tarfile
import time
import urllib.error
import urllib.request
import uuid


def run(args, **kwargs):
    return subprocess.run(args, check=True, text=True, **kwargs)


def output(args, **kwargs):
    return run(args, capture_output=True, **kwargs).stdout.strip()


def request(port, expected):
    deadline = time.monotonic() + 15
    last_error = None
    while time.monotonic() < deadline:
        try:
            opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
            with opener.open(f"http://127.0.0.1:{port}/", timeout=2) as response:
                actual = json.load(response)
                if actual != expected:
                    raise AssertionError(f"actual image response differs: {actual}")
                return actual
        except (OSError, urllib.error.URLError) as error:
            last_error = error
            time.sleep(0.2)
    raise RuntimeError("actual image HTTP listener did not become ready") from last_error


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output-root", type=Path, required=True)
    args = parser.parse_args()
    root = args.output_root.resolve()
    root.mkdir(mode=0o700, parents=True, exist_ok=False)
    source = Path(__file__).resolve().parent
    attempt = uuid.uuid4().hex
    env = os.environ.copy()
    env.update(CGO_ENABLED="0", GOOS="linux", GOARCH="arm64", GOTOOLCHAIN="local", GOPROXY="off", GOSUMDB="off")
    records = []
    tags = {}
    for label, version in [("A", "1.0.0"), ("B", "2.0.0"), ("fail", "3.0.0")]:
        context = root / f"context-{label}"
        context.mkdir()
        run(["go", "build", "-trimpath", "-buildvcs=false", "-ldflags", f"-s -w -X main.imageVersion={version}",
             "-o", str(context / "server"), str(source / "main.go")], env=env)
        binary = (context / "server").read_bytes()
        if binary[:4] != b"\x7fELF" or int.from_bytes(binary[18:20], "little") != 183:
            raise AssertionError("fixture binary is not Linux ARM64 ELF")
        (context / "version.txt").write_text(version + "\n")
        tag = f"localhost/wendy-e2e-small:{attempt}-{label.lower()}"
        archive = root / f"{label}.tar"
        run(["docker", "buildx", "build", "--platform", "linux/arm64", "--network", "none",
             "--provenance=false", "--sbom=false", "--build-arg", f"APP_VERSION={version}",
             "--build-arg", "SOURCE_DATE_EPOCH=0", "--tag", tag,
             "--output", f"type=oci,dest={archive}", "--output", "type=docker",
             "--file", str(source / "Dockerfile"), str(context)])
        layout = root / label
        layout.mkdir()
        with tarfile.open(archive) as tar:
            tar.extractall(layout, filter="data")
        index = json.loads((layout / "index.json").read_text())
        manifests = index["manifests"]
        if len(manifests) != 1:
            raise AssertionError("one ARM64 manifest required")
        digest = manifests[0]["digest"].split(":")[1]
        manifest_bytes = (layout / "blobs/sha256" / digest).read_bytes()
        if hashlib.sha256(manifest_bytes).hexdigest() != digest:
            raise AssertionError("OCI manifest digest mismatch")
        manifest = json.loads(manifest_bytes)
        for descriptor in [manifest["config"], *manifest["layers"]]:
            data = (layout / "blobs/sha256" / descriptor["digest"].split(":")[1]).read_bytes()
            if len(data) != descriptor["size"] or "sha256:" + hashlib.sha256(data).hexdigest() != descriptor["digest"]:
                raise AssertionError("OCI descriptor content mismatch")
        if output(["docker", "run", "--rm", tag, "--version"]) != version:
            raise AssertionError("local image does not contain the baked version")
        image = json.loads(output(["docker", "image", "inspect", tag]))[0]
        if image["Architecture"] != "arm64" or image["Os"] != "linux":
            raise AssertionError("local image architecture mismatch")
        config = json.loads((layout / "blobs/sha256" / manifest["config"]["digest"].split(":")[1]).read_text())
        if image["RootFS"]["Layers"] != config["rootfs"]["diff_ids"]:
            raise AssertionError("loaded image is not the exported OCI filesystem")
        tags[label] = tag
        records.append({"layout": label, "version": version, "tag": tag,
                        "binarySHA256": hashlib.sha256(binary).hexdigest(), "binaryBytes": len(binary),
                        "manifest": "sha256:" + digest, "layers": manifest["layers"]})

    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        port = s.getsockname()[1]
    app = f"sh.wendy.e2e.small-local-{attempt}"
    expected = {"app": app, "attempt": attempt, "unit": "celsius", "fileOnly": "local-file"}
    base_env = {"E2E_APP_ID": app, "E2E_ATTEMPT": attempt, "DISPLAY_UNIT": "celsius",
                "FILE_ONLY": "local-file", "E2E_HTTP_PORT": str(port), "APP_VERSION": "cannot-override-baked-version"}
    responses = []

    def start(tag, extra=None, publish=False):
        command = ["docker", "run", "--detach", "--label", f"sh.wendy.e2e.small={attempt}"]
        if publish:
            command += ["--publish", f"127.0.0.1:{port}:{port}"]
        for key, value in (base_env | (extra or {})).items():
            command += ["--env", f"{key}={value}"]
        return output(command + [tag])

    def remove(container):
        run(["docker", "rm", "--force", container], stdout=subprocess.DEVNULL)

    for label, version in [("A", "1.0.0"), ("B", "2.0.0")]:
        container = start(tags[label], publish=True)
        try:
            responses.append(request(port, expected | {"version": version}))
        finally:
            remove(container)
    boot = output(["docker", "run", "--rm", tags["A"], "--boot-id"])
    uuid.UUID(boot)
    failures = []
    for label, extra in [("fail", {}), ("A", {"E2E_FAIL_ON_BOOT_ID": boot})]:
        container = start(tags[label], extra)
        try:
            waited = run(["docker", "wait", container], capture_output=True, timeout=15)
            if waited.stdout.strip() == "0":
                raise AssertionError("intentional image failure unexpectedly succeeded")
            logs = subprocess.run(["docker", "logs", container], text=True, capture_output=True, check=True)
            text = logs.stdout + logs.stderr
            if "Intentional E2E startup failure" not in text:
                raise AssertionError("own intentional failure diagnostic absent")
            failures.append({"layout": label, "exit": waited.stdout.strip(), "reason": "version" if label == "fail" else "declared boot id"})
        finally:
            remove(container)
    container = start(tags["A"], {"E2E_FAIL_ON_BOOT_ID": str(uuid.uuid4())}, publish=True)
    try:
        responses.append(request(port, expected | {"version": "1.0.0"}))
    finally:
        remove(container)
    report = {"attempt": attempt, "go": output(["go", "version"]), "port": port,
              "images": records, "actualHTTPResponses": responses, "intentionalFailures": failures}
    (root / "local-verification.json").write_text(json.dumps(report, indent=2) + "\n")
    print(json.dumps({"root": str(root), "verified": True, "layerBytes": {r["layout"]: [x["size"] for x in r["layers"]] for r in records}}))


if __name__ == "__main__":
    main()
