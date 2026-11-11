#!/usr/bin/env python3
"""Inspect existing runner credentials and one LAN device without recovery."""

import argparse
import datetime
import ipaddress
import json
import os
from pathlib import Path
import re
import socket
import stat
import subprocess
import sys
import tempfile
from urllib.parse import urlsplit

from ci_diagnostics import failure_summary, stderr_summary


FILES = ("config.json", "devices.json", "known_devices.json", "organization-names.json")
MAX_FILE = 4 * 1024 * 1024
HOST = re.compile(r"[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?"
                  r"(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*\.?")
ATOM = re.compile(r"[A-Za-z0-9_.-]{1,64}")
PROFILE = '''(version 1)
(allow default)
(deny file-write*)
(allow file-write* (subpath (param "STATE")) (literal "/dev/null"))
(deny process-fork)
'''


class DiagnosticError(ValueError):
    """A fixed, locally authored reason that is safe to show in CI."""


def emit(event, **fields):
    # JSON encoding keeps metadata/control characters off the terminal and
    # prevents GitHub workflow-command injection through names or file paths.
    print(json.dumps({"event": event, **fields}, ensure_ascii=True), flush=True)


def hostname(value):
    if len(value) > 253 or not HOST.fullmatch(value) or "." not in value:
        raise ValueError("an explicit dotted LAN hostname or IPv4 address is required")
    return value.lower()


def atom(value):
    return value if isinstance(value, str) and ATOM.fullmatch(value) else "[withheld]"


def pin_host(host):
    # Match config.normalizePinHost / DevicePinFor; do not normalize saved keys.
    return host.strip().lower().removesuffix(".").removesuffix(".local")


def endpoint(value):
    if not value:
        return "absent"
    if not isinstance(value, str) or len(value) > 512:
        return "[withheld]"
    try:
        url = urlsplit(value if "://" in value else "https://" + value)
        if url.scheme not in ("https", "http") or not url.hostname or not HOST.fullmatch(url.hostname):
            return "[withheld]"
        # Paths, credentials, query strings and fragments are never logged.
        return f"{url.scheme}://{url.hostname}" + (f":{url.port}" if url.port else "")
    except ValueError:
        return "[withheld]"


def read_state(source):
    state = {}
    for name in FILES:
        path = source / name
        try:
            mode = path.lstat().st_mode
        except FileNotFoundError:
            emit("file", path=str(path), present=False)
            continue
        emit("file", path=str(path), present=True, mode=oct(stat.S_IMODE(mode)))
        if not stat.S_ISREG(mode):
            raise DiagnosticError("state files must be regular files; no symlinks followed")
        with path.open("rb") as stream:
            data = stream.read(MAX_FILE + 1)
        if len(data) > MAX_FILE:
            raise DiagnosticError("state file exceeds 4 MiB")
        state[name] = data
    return state


def expiry(pem):
    if not isinstance(pem, str) or not pem.startswith("-----BEGIN CERTIFICATE-----"):
        return {"status": "absent_or_unreadable"}
    # Only the leaf and only dates. Never print openssl stderr or certificate data.
    leaf = pem.split("-----END CERTIFICATE-----", 1)[0] + "-----END CERTIFICATE-----\n"
    result = subprocess.run(["/usr/bin/openssl", "x509", "-noout", "-dates"],
                            input=leaf, capture_output=True, text=True, timeout=5)
    if result.returncode:
        return {"status": "unparseable"}
    dates = {}
    for line in result.stdout.splitlines():
        key, _, value = line.partition("=")
        if key not in ("notBefore", "notAfter"):
            continue
        try:
            date = datetime.datetime.strptime(value, "%b %d %H:%M:%S %Y %Z").replace(tzinfo=datetime.timezone.utc)
        except ValueError:
            return {"status": "unparseable"}
        dates[key] = date.isoformat()
        if key == "notAfter":
            dates["expired"] = date <= datetime.datetime.now(datetime.timezone.utc)
    return dates or {"status": "unparseable"}


def oauth_expiry(value):
    if not value:
        return "absent"
    try:
        date = datetime.datetime.fromisoformat(value.replace("Z", "+00:00"))
        if date.tzinfo is not None:
            return date.astimezone(datetime.timezone.utc).isoformat()
    except (ValueError, TypeError, AttributeError):
        pass
    return "unparseable"


def metadata(state, host):
    cfg = json.loads(state.get("config.json", b"{}"))
    pins = cfg.get("devicePins") or {}
    key = pin_host(host)
    binding = pins.get(key) or {}
    emit("context", current=atom(cfg.get("currentContext", "")),
         hostname=host, saved_binding_entry_present=key in pins,
         saved_binding_asset_present=bool(binding.get("assetId")),
         saved_binding_principal_present=bool(binding.get("principal")))
    auths = cfg.get("auth", [])
    if not isinstance(auths, list) or len(auths) > 16:
        raise DiagnosticError("invalid or excessive auth contexts")
    for index, auth in enumerate(auths):
        emit("credential", index=index, context=atom(auth.get("name", "")),
             oauth_expires_at=oauth_expiry(auth.get("oauthExpiresAt")),
             endpoints={key: endpoint(auth.get(key)) for key in (
                 "cloudGRPC", "cloudDashboard", "oauthIssuer", "pkiEndpoint", "oauthResource", "pkiResource")},
             # Values are never emitted, even when stored inline.
             material_present={key: bool(auth.get(key)) for key in (
                 "apiKey", "refreshToken", "dpopPrivateKey", "litePrivateKey", "registryPrivateKey")})
        certs = auth.get("certificates", [])
        if not isinstance(certs, list) or len(certs) > 16:
            raise DiagnosticError("invalid or excessive certificates")
        for cert_index, cert in enumerate(certs):
            emit("certificate", context_index=index, index=cert_index,
                 key_present=bool(cert.get("pemPrivateKey")), **expiry(cert.get("pemCertificate")))
    # file mode disables new Keychain storage but does not disable reads of old
    # references. Refuse rather than probing or migrating the real Keychain.
    # Inspect decoded JSON too: an escaped prefix must not bypass this check.
    if "keychain:" in json.dumps(cfg):
        raise DiagnosticError("Keychain references present; credential probe refused")
    return cfg


def reachability(host):
    # Called in a separate bounded process so resolver stalls cannot hang CI.
    try:
        addresses = sorted({row[4][0] for row in socket.getaddrinfo(host, None, type=socket.SOCK_STREAM)})[:8]
    except OSError:
        emit("resolution", hostname=host, status="failed")
        return
    emit("resolution", hostname=host, addresses=addresses)
    for address in addresses:
        ipaddress.ip_address(address.split("%", 1)[0])
        for port in (50051, 50052):
            try:
                with socket.create_connection((address, port), timeout=2):
                    emit("tcp", address=address, port=port, status="reachable")
            except OSError:
                emit("tcp", address=address, port=port, status="unreachable")


def diagnose(wendy, host, source):
    emit("scope", hostname=host, uid=os.getuid(), secret_store="file",
         network_context="LaunchAgent; no SSH; differs from gate localhost session",
         proof="metadata and read-only device info only; no app RPC, fixtures, repair or enrollment proof")
    state = read_state(source)
    cfg = metadata(state, host)
    # Separate subprocess, inherited environment, no CLI/credentials involved.
    try:
        subprocess.run([sys.executable, __file__, "--reachability", "--hostname", host],
                       check=True, timeout=15)
    except subprocess.TimeoutExpired:
        emit("reachability", status="timeout")
    with tempfile.TemporaryDirectory(prefix="wendy-diagnose-") as scratch:
        target = Path(scratch).resolve()
        for name, data in state.items():
            (target / name).write_bytes(data)
            (target / name).chmod(0o600)
        # Housekeeping can otherwise refresh MCP clients outside ConfigDir.
        # Only this disposable copy changes; auth material and pins stay intact.
        cfg.pop("lastMCPSetupVersion", None)
        (target / "config.json").write_text(json.dumps(cfg))
        (target / "config.json").chmod(0o600)
        env = {"PATH": os.environ.get("PATH", "/usr/bin:/bin"),
               "HOME": str(Path.home()), "TMPDIR": str(target), "CI": "true",
               "WENDY_CONFIG_DIR": str(target), "WENDY_SECRET_STORE": "file",
               "WENDY_NO_UPDATE_CHECK": "1", "WENDY_ANALYTICS": "false"}
        # Existing --read-only bypasses recoverAgentConnection entirely for
        # explicit LAN targets: no OAuth renewal, time sync, picker or updates.
        # Sandbox is additional protection against persistent writes and detached
        # session-broker startup. Missing/unusable sandbox fails closed.
        command = ["/usr/bin/sandbox-exec", "-D", f"STATE={target}", "-p", PROFILE,
                   str(Path(wendy).resolve()), "device", "info", "--read-only",
                   "--device", host, "--json"]
        try:
            result = subprocess.run(command, env=env, stdin=subprocess.DEVNULL,
                                    capture_output=True, text=True, timeout=45, check=True)
        except subprocess.SubprocessError as error:
            emit("device-info", status="failed", detail=failure_summary(error))
            return 1
        # Do not print the hardware payload, which can carry arbitrary peer data.
        payload = json.loads(result.stdout)
        if not isinstance(payload, dict) or "error" in payload:
            raise DiagnosticError("device info did not return a success object")
        emit("device-info", status="success", stderr=stderr_summary(result.stderr))
    return 0


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--wendy")
    parser.add_argument("--hostname", required=True, type=hostname)
    parser.add_argument("--reachability", action="store_true", help=argparse.SUPPRESS)
    args = parser.parse_args()
    try:
        if args.reachability:
            reachability(args.hostname)
            return 0
        if not args.wendy:
            parser.error("--wendy is required")
        source = Path(os.environ.get("WENDY_CONFIG_DIR") or Path.home() / ".wendy")
        if not source.is_absolute():
            raise DiagnosticError("WENDY_CONFIG_DIR must be absolute")
        return diagnose(args.wendy, args.hostname, source)
    except (OSError, ValueError, TypeError, AttributeError, subprocess.SubprocessError) as error:
        emit("diagnostic", status="failed", error_class=type(error).__name__,
             reason=str(error) if isinstance(error, DiagnosticError) else "untrusted details withheld")
        return 1


if __name__ == "__main__":
    sys.exit(main())
