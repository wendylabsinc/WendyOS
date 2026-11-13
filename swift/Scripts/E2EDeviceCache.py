"""Prepare a target-only address fixture using the real CLI in a disposable HOME."""

import argparse
import ipaddress
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import time
from urllib.parse import urlsplit

PREFLIGHT_TIMEOUT_SECONDS = 30


def normalized_host(value):
    return value.lower().rstrip(".").removesuffix(".local")


def target_cache(document, target):
    if not isinstance(document, dict) or document.get("version") != 1:
        raise ValueError("unsupported device cache version")
    host = urlsplit("//" + target).hostname
    entries = [
        entry for entry in document["devices"]
        if normalized_host(entry["hostname"]) == normalized_host(host)
    ]
    if len(entries) != 1:
        raise ValueError("device cache must contain exactly one matching target")
    entry = entries[0]
    ipaddress.ip_address(entry["ip"].split("%", 1)[0])
    if type(entry["port"]) is not int or not 1 <= entry["port"] <= 65535:
        raise ValueError("invalid cached port")
    if entry.get("mtls") is not True:
        raise ValueError("device cache fixture requires an authenticated mTLS target")
    # Keep the CLI's complete entry, including org, mTLS, interface and age.
    return {"version": 1, "devices": entries}


def prepare(cli, auth_config, target, fixture, output):
    started = time.monotonic()
    fixture = Path(fixture)
    if fixture.resolve() in {
        (Path.home() / ".wendy/devices.json").resolve(),
        (Path.home() / ".wendy/config.json").resolve(),
    }:
        raise ValueError("use a dedicated E2E fixture, not the personal device inventory")
    if fixture.resolve() == Path(output).resolve():
        raise ValueError("prepared fixture and run snapshot must be separate files")
    seed = None
    if fixture.exists():
        document = json.loads(fixture.read_text())
        if not isinstance(document, dict) or document.get("e2eTarget") != target or len(document["devices"]) != 1:
            raise ValueError("fixture must be prepared for this explicit target only")
        seed = target_cache(document, target)

    with tempfile.TemporaryDirectory(prefix="wendy-e2e-cache-preflight-") as home:
        config_dir = Path(home) / ".wendy"
        config_dir.mkdir(mode=0o700)
        shutil.copyfile(auth_config, config_dir / "config.json")
        (config_dir / "config.json").chmod(0o600)
        if seed is not None:
            (config_dir / "devices.json").write_text(json.dumps(seed))
        # No TLS-session cache, private keys or pin store are copied from HOME.
        result = subprocess.run(
            [cli, "--json", "--device", target, "device", "info"],
            env={**os.environ, "HOME": home, "WENDY_CONFIG_DIR": str(config_dir), "WENDY_ANALYTICS": "false"},
            check=True, stdout=subprocess.PIPE, text=True, timeout=PREFLIGHT_TIMEOUT_SECONDS,
        )
        json.loads(result.stdout)  # A successful preflight must return JSON.
        cache = target_cache(json.loads((config_dir / "devices.json").read_text()), target)

    output = Path(output)
    output.parent.mkdir(parents=True, exist_ok=True)
    output.write_text(json.dumps(cache) + "\n")
    output.chmod(0o600)
    if seed is None:
        fixture.parent.mkdir(parents=True, exist_ok=True)
        # Publish a complete immutable seed. Concurrent preparations never
        # overwrite an existing fixture; every run consumes its own snapshot.
        with tempfile.NamedTemporaryFile(mode="w", dir=fixture.parent) as temporary:
            json.dump({**cache, "e2eTarget": target}, temporary)
            temporary.flush()
            try:
                os.link(temporary.name, fixture)
            except FileExistsError:
                raise ValueError("fixture appeared during preparation; retry the run")
    print(result.stdout, end="")
    print(f"==> Device cache preflight ({'seeded' if seed else 'cold'}): {time.monotonic() - started:.3f}s", file=sys.stderr)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cli", required=True)
    parser.add_argument("--auth-config", required=True)
    parser.add_argument("--target", required=True)
    parser.add_argument("--fixture", required=True)
    parser.add_argument("--output", required=True)
    args = parser.parse_args()
    try:
        prepare(args.cli, args.auth_config, args.target, args.fixture, args.output)
    except (OSError, ValueError, KeyError, TypeError, subprocess.SubprocessError) as error:
        print(f"ERROR: device cache fixture preflight failed: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
