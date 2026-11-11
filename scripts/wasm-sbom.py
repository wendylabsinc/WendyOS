#!/usr/bin/env python3
"""Inventory modules imported by the release WASM target using its build environment."""

import argparse
import datetime
# SPDX 2.3 requires SHA1 on file records. SHA256 is used for all integrity checks.
import hashlib
import json
from pathlib import Path
import subprocess
from urllib.parse import quote


def modules_from_packages(text):
    decoder = json.JSONDecoder()
    modules = {}
    while text.strip():
        package, end = decoder.raw_decode(text.lstrip())
        text = text.lstrip()[end:]
        module = package.get("Module")
        if not module or module.get("Main"):
            continue
        module = module.get("Replace", module)
        if not module.get("Version") or not module.get("Sum"):
            raise ValueError(f"unversioned or unhashed WASM dependency: {module}")
        modules[module["Path"]] = module
    if not modules:
        raise ValueError("no WASM dependencies resolved")
    return modules


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("binary", type=Path)
    parser.add_argument("output", type=Path)
    args = parser.parse_args()
    # The caller sets GOOS/GOARCH exactly as in build-wasm.mjs.
    text = subprocess.check_output(["go", "list", "-deps", "-json", "./go/cmd/wendy-web"], text=True)
    modules = modules_from_packages(text)
    version = subprocess.check_output(["go", "env", "GOVERSION"], text=True).strip().removeprefix("go")
    modules["stdlib"] = {"Path": "stdlib", "Version": version, "Sum": "Go toolchain " + version}
    packages = []
    for index, module in enumerate(modules.values()):
        packages.append({
            "SPDXID": f"SPDXRef-Module-{index}", "name": module["Path"],
            "versionInfo": module["Version"], "filesAnalyzed": False,
            "downloadLocation": "NOASSERTION", "sourceInfo": module["Sum"],
            "licenseConcluded": "NOASSERTION", "licenseDeclared": "NOASSERTION",
            "copyrightText": "NOASSERTION",
            "externalRefs": [{"referenceCategory": "PACKAGE-MANAGER", "referenceType": "purl",
                              "referenceLocator": f"pkg:golang/{quote(module['Path'], safe='/')}@{quote(module['Version'], safe='')}"}],
        })
    digest = hashlib.sha256(args.binary.read_bytes()).hexdigest()
    document = {
        "spdxVersion": "SPDX-2.3", "dataLicense": "CC0-1.0", "SPDXID": "SPDXRef-DOCUMENT",
        "name": "wendy-web", "documentNamespace": f"https://wendy.dev/sbom/wasm/{digest}",
        "creationInfo": {"created": datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
                         "creators": ["Tool: wendy-wasm-sbom"]},
        "packages": packages,
        "files": [{"SPDXID": "SPDXRef-WASM", "fileName": "wendy.wasm",
                   "checksums": [{"algorithm": "SHA256", "checksumValue": digest},
                                 {"algorithm": "SHA1", "checksumValue": hashlib.sha1(args.binary.read_bytes(), usedforsecurity=False).hexdigest()}]}],
        "relationships": [{"spdxElementId": "SPDXRef-DOCUMENT", "relationshipType": "DESCRIBES",
                           "relatedSpdxElement": "SPDXRef-WASM"}] + [{"spdxElementId": "SPDXRef-WASM", "relationshipType": "DEPENDS_ON",
                           "relatedSpdxElement": p["SPDXID"]} for p in packages],
    }
    args.output.write_text(json.dumps(document, indent=2) + "\n")


if __name__ == "__main__":
    main()
