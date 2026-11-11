#!/usr/bin/env python3
"""Record native libraries that Go build metadata cannot identify after static linking."""

import argparse
import datetime
# SPDX 2.3 requires SHA1 on file records. SHA256 is used for all integrity checks.
import hashlib
import json
from pathlib import Path
import re


def inventory(binary, components):
    digest = hashlib.sha256(binary.read_bytes()).hexdigest()
    packages = []
    vendors = {"libusb": "libusb", "musl": "musl-libc"}
    for component in components:
        name, version = component.split("=", 1)
        if name not in vendors or not re.fullmatch(r"[0-9]+(?:\.[0-9]+)+", version):
            raise ValueError(f"invalid native component: {component}")
        packages.append({
            "SPDXID": f"SPDXRef-{name}", "name": name, "versionInfo": version,
            "filesAnalyzed": False, "downloadLocation": "NOASSERTION",
            "licenseConcluded": "NOASSERTION", "licenseDeclared": "NOASSERTION",
            "copyrightText": "NOASSERTION",
            "externalRefs": [
                {"referenceCategory": "PACKAGE-MANAGER", "referenceType": "purl",
                 "referenceLocator": f"pkg:generic/{name}@{version}"},
                {"referenceCategory": "SECURITY", "referenceType": "cpe23Type",
                 "referenceLocator": f"cpe:2.3:a:{vendors[name]}:{name}:{version}:*:*:*:*:*:*:*"},
            ],
        })
    if not packages:
        raise ValueError("native inventory is empty")
    return {
        "spdxVersion": "SPDX-2.3", "dataLicense": "CC0-1.0", "SPDXID": "SPDXRef-DOCUMENT",
        "name": binary.parent.name + "-native", "documentNamespace": f"https://wendy.dev/sbom/native/{digest}",
        "creationInfo": {"created": datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
                         "creators": ["Tool: wendy-native-sbom"]},
        "packages": packages,
        "files": [{"SPDXID": "SPDXRef-Binary", "fileName": binary.name,
                   "checksums": [{"algorithm": "SHA256", "checksumValue": digest},
                                 {"algorithm": "SHA1", "checksumValue": hashlib.sha1(binary.read_bytes(), usedforsecurity=False).hexdigest()}]}],
        "relationships": [{"spdxElementId": "SPDXRef-DOCUMENT", "relationshipType": "DESCRIBES",
                           "relatedSpdxElement": "SPDXRef-Binary"}] + [{"spdxElementId": "SPDXRef-Binary", "relationshipType": "DEPENDS_ON",
                           "relatedSpdxElement": p["SPDXID"]} for p in packages],
    }


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("binary", type=Path)
    parser.add_argument("output", type=Path)
    parser.add_argument("components", nargs="+")
    args = parser.parse_args()
    args.output.write_text(json.dumps(inventory(args.binary, args.components), indent=2) + "\n")
