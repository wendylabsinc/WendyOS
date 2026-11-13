#!/usr/bin/env python3
"""Inventory the exact SwiftPM resolution used by the macOS release build."""

import argparse
import datetime
# SPDX 2.3 requires SHA1 on file records. SHA256 is used for all integrity checks.
import hashlib
import json
from pathlib import Path
import re
from urllib.parse import quote, urlparse


def inventory(resolved, manifest, workspace_state=None):
    raw = resolved.read_bytes()
    lock = json.loads(raw)
    if lock.get("version") not in (2, 3) or not lock.get("pins"):
        raise ValueError("Package.resolved must contain SwiftPM v2/v3 dependency pins")
    pins = lock["pins"]
    identities = {p["identity"] for p in pins}
    if len(identities) != len(pins):
        raise ValueError("duplicate Swift dependency identities")
    expected = {
        url.rstrip("/").removesuffix(".git").rsplit("/", 1)[-1].lower()
        for url in re.findall(r'\.package\(\s*url:\s*"([^"]+)"', manifest.read_text())
    }
    if not expected or expected - identities:
        raise ValueError(f"missing direct Swift dependencies: {sorted(expected - identities)}")
    if workspace_state is not None:
        dependencies = json.loads(workspace_state.read_text())["object"]["dependencies"]
        checkouts = {
            dependency["packageRef"]["identity"]: dependency["state"]["checkoutState"]["revision"]
            for dependency in dependencies
            if dependency["packageRef"]["kind"] == "remoteSourceControl"
        }
        revisions = {pin["identity"]: pin["state"].get("revision") for pin in pins}
        if not checkouts or revisions != checkouts:
            raise ValueError("Swift resolution does not match Xcode's dependency checkouts")
    packages = []
    for pin in pins:
        state = pin["state"]
        revision = state.get("revision", "")
        if not re.fullmatch(r"[0-9a-f]{40}", revision):
            raise ValueError(f"missing/invalid revision for {pin['identity']}")
        version = state.get("version") or revision
        url = urlparse(pin["location"])
        if pin.get("kind") != "remoteSourceControl" or url.scheme != "https" or not url.netloc:
            raise ValueError(f"unsupported Swift dependency location: {pin['location']}")
        name = pin["identity"]
        purl = f"pkg:swift/{url.netloc}{url.path.removesuffix('.git')}@{quote(version, safe='')}"
        packages.append({
            "SPDXID": f"SPDXRef-Package-{name}", "name": name,
            "versionInfo": version, "filesAnalyzed": False,
            "downloadLocation": f"git+{pin['location']}@{revision}",
            "sourceInfo": f"Package.resolved revision {revision}",
            "licenseConcluded": "NOASSERTION", "licenseDeclared": "NOASSERTION",
            "copyrightText": "NOASSERTION",
            "externalRefs": [{"referenceCategory": "PACKAGE-MANAGER",
                              "referenceType": "purl", "referenceLocator": purl}],
        })
    digest = hashlib.sha256(raw).hexdigest()
    return {
        "spdxVersion": "SPDX-2.3", "dataLicense": "CC0-1.0", "SPDXID": "SPDXRef-DOCUMENT",
        "name": "wendy-swift", "documentNamespace": f"https://wendy.dev/sbom/swift/{digest}",
        "creationInfo": {"created": datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
                         "creators": ["Tool: wendy-swift-sbom"]},
        "packages": packages,
        "files": [{"SPDXID": "SPDXRef-Resolution", "fileName": "Package.resolved",
                   "checksums": [{"algorithm": "SHA256", "checksumValue": digest},
                                 {"algorithm": "SHA1", "checksumValue": hashlib.sha1(raw, usedforsecurity=False).hexdigest()}]}],
        "relationships": [{"spdxElementId": "SPDXRef-DOCUMENT", "relationshipType": "DESCRIBES",
                           "relatedSpdxElement": p["SPDXID"]} for p in packages],
    }


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("resolved", type=Path)
    parser.add_argument("manifest", type=Path)
    parser.add_argument("output", type=Path)
    parser.add_argument("--workspace-state", type=Path)
    args = parser.parse_args()
    result = inventory(args.resolved, args.manifest, args.workspace_state)
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(result, indent=2) + "\n")


if __name__ == "__main__":
    main()
