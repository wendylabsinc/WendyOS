#!/usr/bin/env python3
"""Validate Wendy Agent Developer ID profiles and signed entitlements."""

import argparse
import datetime
import hashlib
import plistlib
import subprocess
import sys
from pathlib import Path
from typing import Any, Dict, Iterable, Optional

TEAM_IDENTIFIER = "3YVC792H3S"
NETWORK_EXTENSION_ENTITLEMENT = "com.apple.developer.networking.networkextension"
SYSTEM_EXTENSION_ENTITLEMENT = "com.apple.developer.system-extension.install"
VIRTUALIZATION_ENTITLEMENT = "com.apple.security.virtualization"
REQUIRED_NETWORK_EXTENSION_VALUES = {
    "app-proxy-provider-systemextension",
    "packet-tunnel-provider-systemextension",
}
LEGACY_NETWORK_EXTENSION_VALUES = {
    "app-proxy-provider",
    "packet-tunnel-provider",
}


class ValidationError(Exception):
    pass


def require(condition: bool, message: str) -> None:
    if not condition:
        raise ValidationError(message)


def validate_network_entitlement(
    entitlements: Dict[str, Any], *, exact: bool
) -> None:
    values = entitlements.get(NETWORK_EXTENSION_ENTITLEMENT)
    require(isinstance(values, list), f"missing {NETWORK_EXTENSION_ENTITLEMENT}")
    value_set = set(values)
    missing = REQUIRED_NETWORK_EXTENSION_VALUES - value_set
    require(not missing, f"missing Network Extension values: {sorted(missing)}")
    legacy = LEGACY_NETWORK_EXTENSION_VALUES & value_set
    require(not legacy, f"contains non-Developer-ID Network Extension values: {sorted(legacy)}")
    if exact:
        require(
            value_set == REQUIRED_NETWORK_EXTENSION_VALUES,
            "signed Network Extension entitlement grants unexpected values: "
            f"{sorted(value_set - REQUIRED_NETWORK_EXTENSION_VALUES)}",
        )


def validate_capabilities(
    entitlements: Dict[str, Any], *, kind: str, exact_network: bool
) -> None:
    validate_network_entitlement(entitlements, exact=exact_network)
    if kind == "host":
        require(
            entitlements.get(SYSTEM_EXTENSION_ENTITLEMENT) is True,
            f"missing {SYSTEM_EXTENSION_ENTITLEMENT}",
        )
        require(
            entitlements.get(VIRTUALIZATION_ENTITLEMENT) is True,
            f"missing {VIRTUALIZATION_ENTITLEMENT}",
        )
    else:
        require(
            SYSTEM_EXTENSION_ENTITLEMENT not in entitlements,
            f"NetProxy must not request {SYSTEM_EXTENSION_ENTITLEMENT}",
        )


def validate_profile(
    profile: Dict[str, Any],
    *,
    bundle_identifier: str,
    kind: str,
    signing_identity: Optional[str],
    now: Optional[datetime.datetime] = None,
) -> None:
    require(profile.get("TeamIdentifier") == [TEAM_IDENTIFIER], "wrong profile team")
    require("OSX" in profile.get("Platform", []), "profile does not support macOS")
    require(profile.get("ProvisionsAllDevices") is True, "profile is not for Developer ID")

    expiration = profile.get("ExpirationDate")
    require(isinstance(expiration, datetime.datetime), "profile has no expiration date")
    comparison_now = now or datetime.datetime.now(datetime.timezone.utc)
    if expiration.tzinfo is None:
        expiration = expiration.replace(tzinfo=datetime.timezone.utc)
    require(expiration > comparison_now, "profile is expired")

    entitlements = profile.get("Entitlements")
    require(isinstance(entitlements, dict), "profile has no entitlements")
    expected_application_identifier = f"{TEAM_IDENTIFIER}.{bundle_identifier}"
    require(
        entitlements.get("com.apple.application-identifier")
        == expected_application_identifier,
        f"profile is for the wrong application identifier; expected {expected_application_identifier}",
    )
    require(
        entitlements.get("com.apple.developer.team-identifier") == TEAM_IDENTIFIER,
        "profile entitlement has the wrong team",
    )
    validate_capabilities(entitlements, kind=kind, exact_network=False)

    if signing_identity:
        normalized_identity = signing_identity.replace(" ", "").upper()
        require(
            len(normalized_identity) == 40
            and all(character in "0123456789ABCDEF" for character in normalized_identity),
            "signing identity must be a SHA-1 certificate fingerprint",
        )
        certificate_fingerprints = {
            hashlib.sha1(certificate).hexdigest().upper()
            for certificate in profile.get("DeveloperCertificates", [])
        }
        require(
            normalized_identity in certificate_fingerprints,
            "profile does not authorize the selected Developer ID certificate",
        )


def decode_profile(path: Path) -> Dict[str, Any]:
    result = subprocess.run(
        ["security", "cms", "-D", "-i", str(path)],
        check=True,
        capture_output=True,
    )
    return plistlib.loads(result.stdout)


def load_plist(path: Path) -> Dict[str, Any]:
    with path.open("rb") as file:
        return plistlib.load(file)


def parse_arguments(arguments: Optional[Iterable[str]] = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    source = parser.add_mutually_exclusive_group(required=True)
    source.add_argument("--profile", type=Path)
    source.add_argument("--entitlements", type=Path)
    parser.add_argument("--bundle-id", required=True)
    parser.add_argument("--kind", required=True, choices=("host", "net-proxy"))
    parser.add_argument("--signing-identity")
    return parser.parse_args(arguments)


def main() -> int:
    arguments = parse_arguments()
    try:
        if arguments.profile:
            profile = decode_profile(arguments.profile)
            validate_profile(
                profile,
                bundle_identifier=arguments.bundle_id,
                kind=arguments.kind,
                signing_identity=arguments.signing_identity,
            )
        else:
            entitlements = load_plist(arguments.entitlements)
            validate_capabilities(entitlements, kind=arguments.kind, exact_network=True)
    except (ValidationError, OSError, plistlib.InvalidFileException, subprocess.CalledProcessError) as error:
        print(f"error: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
