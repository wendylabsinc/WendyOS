#!/usr/bin/env python3
"""Run Go with the Go 1.27.4 server CertificateRequest correction.

Only the build's overlay changes; the installed toolchain is never modified.
See agent-tls.md before updating the Go version or removing this correction.
"""
import json
import os
from pathlib import Path
import shlex
import shutil
import subprocess
import sys
import tempfile


def main():
    args = sys.argv[1:]
    if not args or args[0] not in ("build", "test", "install"):
        raise SystemExit("usage: with-agent-tls.py {build|test|install} [go arguments]")
    if any(a.startswith("-overlay") for a in args + shlex.split(os.environ.get("GOFLAGS", ""))):
        raise SystemExit("an additional Go overlay must be explicitly reconciled with the TLS overlay")
    env = json.loads(subprocess.check_output(["go", "env", "-json", "GOROOT", "GOVERSION", "GOMODCACHE"]))
    if env["GOVERSION"] != "go1.27.4":
        raise SystemExit("review the agent TLS correction for toolchain " + env["GOVERSION"])
    source = Path(env["GOROOT"]) / "src/crypto/tls/handshake_server_tls13.go"
    old = "\t\tcertReq.supportedSignatureAlgorithmsCert = supportedSignatureAlgorithmsCert(c.vers, c.vers)\n"
    new = old + """\t\t// Wendy: restore server certificate algorithms omitted by Go #82536.
\t\t// Keep the ClientHello middlebox mitigation and the FIPS/version filters.
\t\tfor _, sigAlg := range certReq.supportedSignatureAlgorithms {
\t\t\tif !slices.Contains(certReq.supportedSignatureAlgorithmsCert, sigAlg) {
\t\t\t\tcertReq.supportedSignatureAlgorithmsCert = append(certReq.supportedSignatureAlgorithmsCert, sigAlg)
\t\t\t}
\t\t}
"""
    content = source.read_text()
    if content.count(old) != 1 or '"slices"' not in content:
        raise SystemExit("unexpected Go TLS source; refusing to build an unverified correction")
    with tempfile.TemporaryDirectory(prefix="wendy-agent-tls-") as directory:
        root = Path(directory)
        child_env = os.environ.copy()
        # Downloaded toolchains live in GOMODCACHE, where Go forbids overlays.
        # Use a disposable private copy, never modify the shared module cache.
        if source.is_relative_to(Path(env["GOMODCACHE"])):
            private_root = root / "toolchain"
            shutil.copytree(env["GOROOT"], private_root)
            source = private_root / "src/crypto/tls/handshake_server_tls13.go"
            child_env.update(GOROOT=str(private_root), GOTOOLCHAIN="local")
        replacement = root / source.name
        replacement.write_text(content.replace(old, new))
        overlay = root / "overlay.json"
        overlay.write_text(json.dumps({"Replace": {str(source): str(replacement)}}))
        go = str(Path(env["GOROOT"]) / "bin/go")
        return subprocess.call([go, args[0], "-overlay", str(overlay), *args[1:]], env=child_env)


if __name__ == "__main__":
    sys.exit(main())
