#!/usr/bin/env python3
"""Catalog final release payloads, scan every inventory, and verify attestations."""

import argparse
import copy
import gzip
# SPDX 2.3 requires SHA1 on file records. SHA256 is used for all integrity checks.
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tarfile
import tempfile
import zipfile


EXTENSIONS = (".tar.gz", ".zip", ".deb", ".rpm", ".msi")


def run(*args, **kwargs):
    return subprocess.run([str(a) for a in args], check=True, **kwargs)


def sha256(path):
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def write_json(path, value):
    path.write_text(json.dumps(value, indent=2) + "\n")


def release_files(dist):
    files = sorted(p for p in dist.rglob("wendy*") if p.is_file() and p.name.endswith(EXTENSIONS))
    names = [p.name for p in files]
    if len(names) != len(set(names)):
        raise ValueError("duplicate release asset names")
    # Fail closed if a producer or a download silently omits a product/architecture.
    for product in ("wendy-agent-linux-amd64", "wendy-agent-linux-arm64",
                    "wendy-cli-linux-amd64", "wendy-cli-linux-arm64",
                    "wendy-cli-darwin-arm64", "wendy-macos-runtime-guest-arm64", "wendy-web"):
        if sum(n.startswith(product + "-") and n.endswith(".tar.gz") for n in names) != 1:
            raise ValueError(f"expected one {product} archive")
    for product in ("wendy-cli-windows-amd64", "wendy-cli-windows-arm64", "wendy-agent-macos-arm64"):
        if sum(n.startswith(product + "-") and n.endswith(".zip") for n in names) != 1:
            raise ValueError(f"expected one {product} zip")
    for arch, rpm_arch in (("amd64", "x86_64"), ("arm64", "aarch64")):
        if sum(n.startswith(f"wendy-cli-windows-{arch}-") and n.endswith(".msi") for n in names) != 1:
            raise ValueError(f"expected one {arch} MSI")
        for product in ("wendy", "wendy-agent"):
            if sum(n.startswith(product + "_") and n.endswith(f"_{arch}.deb") for n in names) != 1:
                raise ValueError(f"expected one {product} {arch} DEB")
            if sum(n.startswith(product + "-") and n[len(product) + 1:len(product) + 2].isdigit()
                   and n.endswith(f".{rpm_arch}.rpm") for n in names) != 1:
                raise ValueError(f"expected one {product} {rpm_arch} RPM")
    if len(files) != 20:
        raise ValueError(f"unexpected release asset set: {names}")
    return files


def extract(archive, dest):
    dest.mkdir()
    if archive.name.endswith(".tar.gz"):
        with tarfile.open(archive) as stream:
            stream.extractall(dest, filter="data")
    elif archive.suffix == ".zip":
        with zipfile.ZipFile(archive) as stream:
            for item in stream.infolist():
                target = (dest / item.filename).resolve()
                if not target.is_relative_to(dest.resolve()):
                    raise ValueError(f"unsafe archive member: {item.filename}")
            stream.extractall(dest)
    elif archive.suffix == ".deb":
        run("dpkg-deb", "--raw-extract", archive, dest)
    elif archive.suffix == ".rpm":
        # Keep scriptlets and dependency declarations with the extracted payload.
        with (dest / "rpm-metadata.txt").open("w") as output:
            run("rpm", "-qp", "--requires", archive, stdout=output)
            run("rpm", "-qp", "--scripts", archive, stdout=output)
        run("bsdtar", "-xf", archive, "-C", dest)
    elif archive.suffix == ".msi":
        run("msiextract", "-C", dest, archive)
    else:
        raise ValueError(f"unsupported archive: {archive}")


def catalog(root, output):
    run(os.environ.get("SYFT_BIN", "syft"), "scan", f"dir:{root}", "-o", f"spdx-json={output}")
    doc = json.loads(output.read_text())
    packages = [p for p in doc.get("packages", []) if any(
        ref.get("referenceType") == "purl" for ref in p.get("externalRefs", []))]
    if not packages:
        raise ValueError(f"no dependency packages cataloged in {root}")
    return doc


def merge(documents):
    result = copy.deepcopy(documents[0])
    result["name"] = "wendy-release"
    result["documentNamespace"] += "/release"
    for field in ("packages", "files", "relationships", "documentDescribes", "annotations", "hasExtractedLicensingInfos"):
        result.pop(field, None)
    result.update(packages=[], files=[], relationships=[], hasExtractedLicensingInfos=[])
    for index, document in enumerate(documents):
        def remap(value, key=None):
            if isinstance(value, str) and value.startswith("SPDXRef-"):
                return "SPDXRef-DOCUMENT" if value == document["SPDXID"] else f"SPDXRef-{index}-{value[8:]}"
            if isinstance(value, str) and key in {
                "licenseId", "licenseConcluded", "licenseDeclared", "licenseInfoFromFiles", "licenseInfoInFiles"
            }:
                return re.sub(r"\bLicenseRef-([A-Za-z0-9.-]+)", rf"LicenseRef-{index}-\1", value)
            if isinstance(value, list):
                return [remap(v, key) for v in value]
            if isinstance(value, dict):
                return {k: remap(v, k) for k, v in value.items()}
            return value
        for field in ("packages", "files", "relationships", "hasExtractedLicensingInfos"):
            result[field].extend(remap(document.get(field, [])))
    return result


def prepare(dist, output, swift):
    assets = release_files(dist)
    output.mkdir(parents=True, exist_ok=True)
    inventories = output / "inventories"
    inventories.mkdir()
    shutil.copyfile(swift, inventories / "swift.spdx.json")
    documents = [json.loads(swift.read_text())]
    if not documents[0].get("packages"):
        raise ValueError("empty Swift inventory")
    native = {}
    for product in ("wendy-cli-linux-amd64", "wendy-cli-linux-arm64", "wendy-cli-darwin-arm64"):
        candidates = list(dist.rglob(product + "-native.spdx.json"))
        if len(candidates) != 1:
            raise ValueError(f"missing native library inventory for {product}")
        path = candidates[0]
        document = json.loads(path.read_text())
        expected = {"libusb", "musl"} if "linux" in product else {"libusb"}
        if {p["name"] for p in document["packages"]} != expected:
            raise ValueError(f"incomplete native library inventory for {product}")
        native[product] = document
        documents.append(document)
        shutil.copyfile(path, inventories / path.name)
    wasm_paths = list(dist.rglob("wendy-web-modules.spdx.json"))
    if len(wasm_paths) != 1:
        raise ValueError("missing WASM module inventory")
    wasm = json.loads(wasm_paths[0].read_text())
    if len(wasm.get("packages", [])) < 2:
        raise ValueError("empty WASM module inventory")
    documents.append(wasm)
    shutil.copyfile(wasm_paths[0], inventories / wasm_paths[0].name)
    coverage = []
    with tempfile.TemporaryDirectory(prefix="release-security-") as temporary:
        for archive in assets:
            root = Path(temporary) / archive.name
            extract(archive, root)
            entry = {"asset": archive.name, "sha256": sha256(archive), "sha1": hashlib.sha1(archive.read_bytes(), usedforsecurity=False).hexdigest(), "inventories": [], "files": []}
            # Hash the installed payload before adding cataloger inputs or unpacking nested images.
            for path in sorted(root.rglob("*")):
                if path.is_file() and not path.is_symlink():
                    entry["files"].append({"path": str(path.relative_to(root)), "sha256": sha256(path)})
            # Match the build-time native inventory to the exact binary, also in DEB/RPM payloads.
            cli_files = [f for f in entry["files"] if Path(f["path"]).name == "wendy"]
            for cli in cli_files:
                matches = [product for product, doc in native.items()
                           if doc["files"][0]["checksums"][0]["checksumValue"] == cli["sha256"]]
                if len(matches) != 1:
                    raise ValueError(f"no matching native inventory for {archive.name}/{cli['path']}")
                entry["inventories"].append(matches[0] + "-native.spdx.json")
            if archive.name.startswith("wendy-agent-macos-"):
                entry["inventories"].append("swift.spdx.json")
            else:
                if archive.name.startswith("wendy-web-"):
                    with gzip.open(root / "wendy.wasm.gz", "rb") as source:
                        with (root / "wendy.wasm").open("wb") as target:
                            shutil.copyfileobj(source, target)
                    if sha256(root / "wendy.wasm") != wasm["files"][0]["checksums"][0]["checksumValue"]:
                        raise ValueError("WASM inventory does not match the released binary")
                    entry["inventories"].append(wasm_paths[0].name)
                elif not archive.name.startswith("wendy-macos-runtime-guest-"):
                    name = archive.name + ".spdx.json"
                    documents.append(catalog(root, inventories / name))
                    entry["inventories"].append(name)
            # Syft does not traverse compressed cpio initramfs images itself.
            images = list(root.rglob("initramfs-*.img"))
            if archive.name.startswith(("wendy-macos-runtime-guest-", "wendy-agent-macos-")) and len(images) != 1:
                raise ValueError(f"expected exactly one runtime initramfs in {archive.name}")
            for index, image in enumerate(images):
                guest = Path(temporary) / f"guest-{index}"
                guest.mkdir()
                run("bsdtar", "-xf", image, "-C", guest)
                name = archive.name + ".runtime.spdx.json"
                document = catalog(guest, inventories / name)
                names = {p["name"] for p in document["packages"]}
                if not {"linux-virt", "containerd", "runc", "busybox"} <= names:
                    raise ValueError(f"incomplete runtime inventory in {archive.name}")
                documents.append(document)
                entry["inventories"].append(name)
                shutil.rmtree(guest)
            coverage.append(entry)
            shutil.rmtree(root)
    write_json(output / "release-coverage.json", {"assets": coverage})
    aggregate = merge(documents)
    # Bind original download hashes in the predicate, including installers.
    for index, asset in enumerate(coverage):
        file_id = f"SPDXRef-ReleaseAsset-{index}"
        aggregate["files"].append({"SPDXID": file_id, "fileName": asset["asset"],
                                   "checksums": [{"algorithm": "SHA256", "checksumValue": asset["sha256"]},
                                                 {"algorithm": "SHA1", "checksumValue": asset["sha1"]}]})
        aggregate["relationships"].append({"spdxElementId": "SPDXRef-DOCUMENT",
                                            "relationshipType": "DESCRIBES", "relatedSpdxElement": file_id})
    digest = hashlib.sha256(json.dumps(aggregate, sort_keys=True).encode()).hexdigest()
    aggregate["documentNamespace"] = f"https://wendy.dev/sbom/release/{digest}"
    write_json(output / "wendy-release.spdx.json", aggregate)


def scan(output):
    reports = output / "scans"
    reports.mkdir()
    inventories = sorted((output / "inventories").glob("*.spdx.json"))
    if not inventories:
        raise ValueError("no release inventories to scan")
    failed = []
    for inventory in inventories:
        with (reports / (inventory.name + ".grype.json")).open("w") as report:
            result = subprocess.run([os.environ.get("GRYPE_BIN", "grype"), f"sbom:{inventory}",
                                     "--fail-on", "high", "-o", "json"], stdout=report)
        if result.returncode:
            failed.append({"inventory": inventory.name, "exit_code": result.returncode})
    write_json(output / "release-scan-result.json", {"passed": not failed, "failures": failed,
                                                     "policy": "Block all High/Critical findings; no exceptions"})
    if failed:
        raise ValueError(f"release vulnerability gate failed: {failed}")


def verify(dist, output, repository, bundle):
    expected = json.loads((output / "wendy-release.spdx.json").read_text())
    evidence = []
    for artifact in release_files(dist):
        verified = run("gh", "attestation", "verify", artifact, "--repo", repository,
                       "--signer-workflow", f"{repository}/.github/workflows/build.yml",
                       "--predicate-type", "https://spdx.dev/Document", "--bundle", bundle,
                       "--format", "json", capture_output=True, text=True)
        records = json.loads(verified.stdout)
        if not any(r["verificationResult"]["statement"]["predicate"] == expected for r in records):
            raise ValueError(f"attested SBOM differs from release SBOM: {artifact.name}")
        evidence.append({"asset": artifact.name, "sha256": sha256(artifact), "verified": True})
    write_json(output / "release-attestation-verification.json", evidence)


def publication_files(dist, output):
    """Recheck the scanned hashes and emit one upload path per release asset name."""
    coverage = json.loads((output / "release-coverage.json").read_text())
    expected = {asset["asset"]: asset["sha256"] for asset in coverage["assets"]}
    paths = release_files(dist)
    if {p.name for p in paths} != set(expected):
        raise ValueError("release assets differ from scanned assets")
    for path in paths:
        if sha256(path) != expected[path.name]:
            raise ValueError(f"release artifact changed after scan: {path.name}")
    for pattern in ("*.spdx.json", "*.grype.json", "release-*.json", "*-signatures*", "Package.resolved"):
        paths.extend(p for p in dist.rglob(pattern) if p.is_file())
    by_name = {}
    for path in sorted(paths):
        if path.name in by_name and sha256(path) != sha256(by_name[path.name]):
            raise ValueError(f"conflicting release evidence: {path.name}")
        by_name[path.name] = path
    return sorted(by_name.values())


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("prepare", "scan", "verify", "files"))
    parser.add_argument("--dist", type=Path, default=Path("dist"))
    parser.add_argument("--output", type=Path, default=Path("release-security"))
    parser.add_argument("--swift", type=Path)
    parser.add_argument("--repository")
    parser.add_argument("--bundle", type=Path)
    args = parser.parse_args()
    if args.command == "prepare":
        prepare(args.dist.resolve(), args.output, args.swift)
    elif args.command == "scan":
        scan(args.output)
    elif args.command == "files":
        print("\n".join(str(p) for p in publication_files(args.dist, args.output)))
    else:
        verify(args.dist, args.output, args.repository, args.bundle)


if __name__ == "__main__":
    main()
