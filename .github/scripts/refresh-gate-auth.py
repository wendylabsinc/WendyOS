#!/usr/bin/env python3
"""Refresh only the hardware gate's selected human CI login; never log CLI output."""

import argparse
from datetime import datetime, timezone
import json
import os
import subprocess
import sys


RECOVERY = ("In the runner's login session, run 'WENDY_SECRET_STORE=file wendy auth login' "
            "as ci@wendy.sh, then 'WENDY_SECRET_STORE=file wendy auth use wendy'.")
PRINCIPAL_PREFIX = "spiffe://wendy.sh/tenant/5c9e14bb-49c0-4178-9cc5-493a151f9885/operator/"


def cli(wendy, *args):
    return subprocess.run(
        [wendy, *args, "--json"], stdin=subprocess.DEVNULL,
        stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
        env=dict(os.environ, CI="true", WENDY_SECRET_STORE="file", WENDY_NO_UPDATE_CHECK="1"),
        check=True, timeout=60, text=True,
    ).stdout


def session(wendy):
    status = json.loads(cli(wendy, "auth", "status"))
    current = [s for s in status["sessions"] if s.get("current") is True]
    if status.get("loggedIn") is not True or len(current) != 1:
        raise ValueError("context")
    selected = current[0]
    if (selected.get("context") != "wendy" or
            selected.get("cloudGrpc") != "api.wendy.dev:443" or
            selected.get("loginPath") != "OIDC" or
            not selected.get("principalUri", "").startswith(PRINCIPAL_PREFIX) or
            not selected.get("certificate")):
        raise ValueError("context")
    return selected


def refresh(wendy):
    try:
        before = session(wendy)
    except (OSError, subprocess.SubprocessError, ValueError, KeyError, TypeError, AttributeError):
        print("::error::Cannot verify the gate's selected wendy production OIDC context. " + RECOVERY)
        return 1
    try:
        # The CLI owns rotating-token locking and persistence, and returns
        # nonzero if no context was refreshed. Never use --all or re-login.
        cli(wendy, "auth", "refresh-certs")
    except (OSError, subprocess.SubprocessError, ValueError):
        print("::error::CI certificate refresh failed or timed out. An expired or refused "
              "refresh token needs a human login. " + RECOVERY)
        return 1
    try:
        after = session(wendy)
        cert = after["certificate"]
        expiry = datetime.fromisoformat(cert["expiresAt"].replace("Z", "+00:00"))
        if (after["principalUri"] != before["principalUri"] or
                cert.get("expired") is not False or expiry <= datetime.now(timezone.utc)):
            raise ValueError("certificate")
    except (OSError, subprocess.SubprocessError, ValueError, KeyError, TypeError, AttributeError):
        print("::error::CI refresh did not leave a usable certificate in the intended context. " + RECOVERY)
        return 1
    print("CI certificate refresh completed for the selected wendy production context.")
    return 0


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--wendy", required=True)
    sys.exit(refresh(parser.parse_args().wendy))
