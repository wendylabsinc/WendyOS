#!/usr/bin/env python3
"""Compare stock/fixed TLS 1.3 client authentication using real OpenSSL."""
import json
import os
from pathlib import Path
import socket
import ssl
import subprocess
import sys
import tempfile

ROOT = Path(__file__).resolve().parents[2]
MLDSA = {0x0904, 0x0905, 0x0906}


def connect(directory, port, identity):
    context = ssl.create_default_context(cafile=str(directory / "root.pem"))
    context.minimum_version = ssl.TLSVersion.TLSv1_3
    context.set_alpn_protocols(["h2"])
    if identity:
        context.load_cert_chain(str(directory / (identity + ".pem")),
                                str(directory / (identity + ".key")))
    request = {}

    def observe(_socket, direction, _version, content_type, message_type, data):
        if direction != "read" or int(content_type) != 22 or int(message_type) != 13:
            return
        offset = 5 + data[4]  # handshake header and request context
        end = offset + 2 + int.from_bytes(data[offset:offset + 2], "big")
        offset += 2
        while offset < end:
            kind = int.from_bytes(data[offset:offset + 2], "big")
            length = int.from_bytes(data[offset + 2:offset + 4], "big")
            value = data[offset + 4:offset + 4 + length]
            offset += 4 + length
            if kind in (13, 50):
                request[kind] = {int.from_bytes(value[i:i + 2], "big")
                                 for i in range(2, len(value), 2)}

    context._msg_callback = observe
    try:
        with socket.create_connection(("127.0.0.1", port), timeout=10) as raw:
            with context.wrap_socket(raw, server_hostname="localhost") as tls:
                result = tls.recv(64)
                return result == b"authenticated", request, result.decode(errors="replace")
    except ssl.SSLError as error:
        return False, request, str(error)


def exercise(binary, directory, corrected):
    directory.mkdir()
    proc = subprocess.Popen([str(binary), str(directory)], stdout=subprocess.PIPE,
                            stderr=subprocess.PIPE, text=True)
    try:
        port = json.loads(proc.stdout.readline())["port"]
        for identity in ("mldsa", "ecdsa", None, "untrusted"):
            ok, request, result = connect(directory, port, identity)
            expected = identity == "ecdsa" or (corrected and identity == "mldsa")
            assert ok == expected, (corrected, identity, result)
            assert MLDSA <= request[13], request
            assert (MLDSA <= request[50]) == corrected, request
            # Keep legacy certificate signature algorithms, even though TLS
            # 1.3 does not use them for CertificateVerify signatures.
            assert 0x0401 in request[50], request  # rsa_pkcs1_sha256
            if not corrected and identity == "mldsa":
                assert "CERTIFICATE_REQUIRED" in result, result
            print(json.dumps({"build": "corrected" if corrected else "stock",
                              "identity": identity, "authenticated": ok}), flush=True)
    finally:
        proc.terminate()
        proc.communicate(timeout=10)


def main():
    assert ssl.OPENSSL_VERSION_INFO >= (3, 5), ssl.OPENSSL_VERSION
    env = dict(os.environ, CGO_ENABLED="0")
    # The fixture executes on this host, regardless of a caller's build target.
    for key in ("GOOS", "GOARCH", "GOARM64"):
        env.pop(key, None)
    with tempfile.TemporaryDirectory(prefix="wendy-tls-regression-") as temp:
        work = Path(temp)
        fixture = "./go/scripts/testdata/agent-tls"
        for corrected in (False, True):
            name = "corrected" if corrected else "stock"
            binary = work / name
            command = ([sys.executable, str(ROOT / "go/scripts/with-agent-tls.py")]
                       if corrected else ["go"])
            subprocess.run([*command, "build", "-o", str(binary), fixture], cwd=ROOT,
                           env=env, check=True, timeout=180)
            exercise(binary, work / (name + "-certs"), corrected)
    print("PASS: ML-DSA and legacy authentication; missing/untrusted identities rejected")


if __name__ == "__main__":
    main()
