import copy
import gzip
import importlib.util
import io
import json
import re
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest
from unittest.mock import patch
import zipfile


def load(name):
    spec = importlib.util.spec_from_file_location(name.replace("-", "_"), Path(__file__).parents[1] / (name + ".py"))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


security = load("release-security")
swift = load("swift-sbom")
native = load("native-sbom")
wasm = load("wasm-sbom")


class SwiftInventoryTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.manifest = self.root / "Package.swift"
        self.manifest.write_text('.package(\n url: "https://github.com/apple/swift-log.git", from: "1.6.3")')
        self.resolved = self.root / "Package.resolved"
        self.lock = {"version": 3, "pins": [self.pin("swift-log", "1.6.3"), self.pin("swift-nio-ssl")]}

    def pin(self, name, version=None):
        state = {"revision": "a" * 40}
        if version:
            state["version"] = version
        return {"identity": name, "kind": "remoteSourceControl",
                "location": f"https://github.com/apple/{name}.git", "state": state}

    def inventory(self):
        self.resolved.write_text(json.dumps(self.lock))
        return swift.inventory(self.resolved, self.manifest)

    def test_inventory_preserves_transitive_packages_and_revision_pins(self):
        doc = self.inventory()
        self.assertEqual([p["versionInfo"] for p in doc["packages"]], ["1.6.3", "a" * 40])
        self.assertIn("@" + "a" * 40, doc["packages"][1]["externalRefs"][0]["referenceLocator"])
        self.assertEqual(doc["files"][0]["checksums"][0]["checksumValue"], security.sha256(self.resolved))

    def test_missing_lock_fails(self):
        with self.assertRaises(FileNotFoundError):
            swift.inventory(self.resolved, self.manifest)

    def test_empty_or_unsupported_lock_fails(self):
        for lock in ({"version": 3, "pins": []}, {"version": 7, "pins": self.lock["pins"]}):
            self.lock = lock
            with self.assertRaises(ValueError):
                self.inventory()

    def test_missing_declared_dependency_fails(self):
        self.lock["pins"] = [self.pin("swift-nio-ssl")]
        with self.assertRaisesRegex(ValueError, "missing direct"):
            self.inventory()

    def test_duplicate_or_unresolved_pins_fail(self):
        original = copy.deepcopy(self.lock)
        self.lock["pins"].append(self.lock["pins"][0])
        with self.assertRaisesRegex(ValueError, "duplicate"):
            self.inventory()

        self.lock = original
        self.lock["pins"][0]["state"].pop("revision")
        with self.assertRaisesRegex(ValueError, "revision"):
            self.inventory()

    def test_transitive_pin_must_match_xcode_checkout(self):
        self.inventory()
        workspace = self.root / "workspace-state.json"
        workspace.write_text(json.dumps({"object": {"dependencies": [
            {"packageRef": {"identity": p["identity"], "kind": "remoteSourceControl"},
             "state": {"checkoutState": p["state"]}} for p in self.lock["pins"]
        ]}}))
        swift.inventory(self.resolved, self.manifest, workspace)
        self.lock["pins"].pop()
        self.inventory()
        with self.assertRaisesRegex(ValueError, "checkouts"):
            swift.inventory(self.resolved, self.manifest, workspace)


class ReleaseTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)

    def assets(self):
        names = [f"{p}-1.tar.gz" for p in (
            "wendy-agent-linux-amd64", "wendy-agent-linux-arm64", "wendy-cli-linux-amd64",
            "wendy-cli-linux-arm64", "wendy-cli-darwin-arm64", "wendy-macos-runtime-guest-arm64", "wendy-web")]
        names += [f"{p}-1.zip" for p in ("wendy-cli-windows-amd64", "wendy-cli-windows-arm64", "wendy-agent-macos-arm64")]
        for arch, rpm_arch in (("amd64", "x86_64"), ("arm64", "aarch64")):
            names.append(f"wendy-cli-windows-{arch}-1.msi")
            for product in ("wendy", "wendy-agent"):
                names += [f"{product}_1_{arch}.deb", f"{product}-1.{rpm_arch}.rpm"]
        for name in names:
            (self.root / name).touch()
        return names

    def test_requires_every_product_and_architecture(self):
        names = self.assets()
        self.assertEqual(len(security.release_files(self.root)), 20)
        for name in names:
            (self.root / name).unlink()
            with self.assertRaises(ValueError, msg=name):
                security.release_files(self.root)
            (self.root / name).touch()

    def test_duplicate_and_unmapped_assets_fail(self):
        names = self.assets()
        extra = self.root / "wendy-new-product-1.zip"
        extra.touch()
        with self.assertRaisesRegex(ValueError, "unexpected"):
            security.release_files(self.root)
        extra.unlink()
        (self.root / "duplicate").mkdir()
        (self.root / "duplicate" / names[0]).touch()
        with self.assertRaisesRegex(ValueError, "duplicate"):
            security.release_files(self.root)

    def test_extracts_archives_and_blocks_path_traversal(self):
        for suffix in (".zip", ".tar.gz"):
            for filename in ("product/wendy", "../escape"):
                archive = self.root / ("archive" + suffix)
                if suffix == ".zip":
                    with zipfile.ZipFile(archive, "w") as z:
                        z.writestr(filename, b"binary")
                else:
                    with tarfile.open(archive, "w:gz") as t:
                        member = tarfile.TarInfo(filename)
                        member.size = 6
                        t.addfile(member, io.BytesIO(b"binary"))
                dest = self.root / (suffix + str(filename.startswith("..")))
                if filename.startswith(".."):
                    with self.assertRaises((ValueError, tarfile.FilterError)):
                        security.extract(archive, dest)
                else:
                    security.extract(archive, dest)
                    self.assertEqual((dest / filename).read_bytes(), b"binary")

    def test_catalog_rejects_root_only_inventory(self):
        output = self.root / "empty.spdx.json"
        output.write_text(json.dumps({"packages": [{"name": "root"}]}))
        with patch.object(security, "run"), self.assertRaisesRegex(ValueError, "no dependency"):
            security.catalog(self.root, output)

    def test_merge_remaps_relationships_and_duplicate_ids(self):
        doc = {"SPDXID": "SPDXRef-DOCUMENT", "documentNamespace": "https://example.test/test",
               "packages": [{"SPDXID": "SPDXRef-package", "name": "package"}],
               "relationships": [{"spdxElementId": "SPDXRef-DOCUMENT", "relatedSpdxElement": "SPDXRef-package"}]}
        merged = security.merge([doc, doc])
        ids = {p["SPDXID"] for p in merged["packages"]}
        self.assertEqual(len(ids), 2)
        self.assertEqual({r["relatedSpdxElement"] for r in merged["relationships"]}, ids)

    def test_merge_preserves_and_scopes_custom_license_definitions(self):
        doc = {"SPDXID": "SPDXRef-DOCUMENT", "documentNamespace": "https://example.test/test",
               "packages": [{"SPDXID": "SPDXRef-package", "licenseDeclared": "MIT AND LicenseRef-Custom"}],
               "hasExtractedLicensingInfos": [{"licenseId": "LicenseRef-Custom", "extractedText": "terms"}]}
        merged = security.merge([doc, doc])
        self.assertEqual(merged["packages"][1]["licenseDeclared"], "MIT AND LicenseRef-1-Custom")
        self.assertEqual(merged["hasExtractedLicensingInfos"][1]["licenseId"], "LicenseRef-1-Custom")

    def test_publication_rechecks_hashes_and_deduplicates_evidence(self):
        names = self.assets()
        security.write_json(self.root / "release-coverage.json", {"assets": [
            {"asset": name, "sha256": security.sha256(self.root / name)} for name in names]})
        (self.root / "inventories").mkdir()
        for directory in (self.root, self.root / "inventories"):
            (directory / "native.spdx.json").write_text("{}")
        paths = security.publication_files(self.root, self.root)
        self.assertEqual(sum(p.name == "native.spdx.json" for p in paths), 1)
        (self.root / "inventories/native.spdx.json").write_text('{"changed":true}')
        with self.assertRaisesRegex(ValueError, "conflicting"):
            security.publication_files(self.root, self.root)
        (self.root / names[0]).write_bytes(b"changed after scan")
        with self.assertRaisesRegex(ValueError, "changed after scan"):
            security.publication_files(self.root, self.root)

    def test_all_independent_publishers_require_gate_success(self):
        workflow = (Path(__file__).parents[2] / ".github/workflows/build.yml").read_text()
        jobs = dict(re.findall(r"^  ([\w-]+):\n(.*?)(?=^  [\w-]+:|\Z)", workflow, re.M | re.S))
        for name in ("release", "publish-agent-gcs", "publish-install-scripts", "publish-linux-repos"):
            job = jobs[name]
            needs = next(line for line in job.splitlines() if line.startswith("    needs:"))
            self.assertIn("release-security", needs, name)
            self.assertIn("needs.release-security.result == 'success'", job, name)

    def test_prepare_maps_all_payloads_and_supplemental_inventories(self):
        self.assets()
        binary = self.root / "binary"
        for product in ("wendy-cli-linux-amd64", "wendy-cli-linux-arm64", "wendy-cli-darwin-arm64"):
            binary.write_bytes(product.encode())
            components = ["libusb=1.0.29"] + (["musl=1.2.5"] if "linux" in product else [])
            security.write_json(self.root / (product + "-native.spdx.json"), native.inventory(binary, components))
        binary.write_bytes(b"wasm")
        base = native.inventory(binary, ["libusb=1.0.29", "musl=1.2.5"])
        security.write_json(self.root / "wendy-web-modules.spdx.json", base)
        swift_path = self.root / "swift.spdx.json"
        security.write_json(swift_path, base)

        def extract(archive, dest):
            dest.mkdir()
            name = archive.name
            if name.startswith(("wendy-cli-linux-", "wendy-cli-darwin-")):
                (dest / "wendy").write_bytes(name.removesuffix("-1.tar.gz").encode())
            elif name.startswith(("wendy_", "wendy-1.")):
                arch = "amd64" if any(a in name for a in ("amd64", "x86_64")) else "arm64"
                (dest / "wendy").write_bytes(f"wendy-cli-linux-{arch}".encode())
            elif name.startswith(("wendy-agent-macos-", "wendy-macos-runtime-guest-")):
                (dest / "initramfs-arm64.img").write_bytes(b"image")
            elif name.startswith("wendy-web-"):
                (dest / "wendy.wasm.gz").write_bytes(gzip.compress(b"wasm"))
            else:
                (dest / "wendy.exe").write_bytes(b"binary")

        def catalog(root, output):
            doc = copy.deepcopy(base)
            doc["packages"] = [{"SPDXID": f"SPDXRef-{name}", "name": name}
                               for name in ("linux-virt", "containerd", "runc", "busybox")]
            security.write_json(output, doc)
            return doc

        output = self.root / "evidence"
        with patch.object(security, "extract", side_effect=extract), patch.object(security, "catalog", side_effect=catalog), patch.object(security, "run"):
            security.prepare(self.root, output, swift_path)
        assets = json.loads((output / "release-coverage.json").read_text())["assets"]
        self.assertEqual(len(assets), 20)
        for asset in assets:
            self.assertTrue(asset["inventories"], asset["asset"])
            for name in asset["inventories"]:
                self.assertTrue((output / "inventories" / name).exists(), name)
        security.publication_files(self.root, output)

    def test_scan_failures_block_but_retain_all_reports(self):
        inventories = self.root / "inventories"
        inventories.mkdir()
        for name in ("a", "b", "c"):
            (inventories / (name + ".spdx.json")).write_text("{}")
        def scanner(command, stdout):
            self.assertEqual(command[-4:], ["--fail-on", "high", "-o", "json"])
            stdout.write('{"matches":[]}')
            # Both a vulnerability exit and a scanner/database error must block.
            return subprocess.CompletedProcess(command, 2 if "a.spdx" in command[1] else 1)
        with patch.object(security.subprocess, "run", side_effect=scanner), self.assertRaises(ValueError):
            security.scan(self.root)
        self.assertEqual(len(list((self.root / "scans").glob("*.json"))), 3)
        result = json.loads((self.root / "release-scan-result.json").read_text())
        self.assertFalse(result["passed"])
        self.assertEqual(len(result["failures"]), 3)

    def test_clean_scan_passes_and_empty_inventory_fails(self):
        (self.root / "inventories").mkdir()
        with self.assertRaisesRegex(ValueError, "no release inventories"):
            security.scan(self.root)
        (self.root / "scans").rmdir()
        (self.root / "inventories/a.spdx.json").write_text("{}")
        with patch.object(security.subprocess, "run", return_value=subprocess.CompletedProcess([], 0)):
            security.scan(self.root)
        self.assertTrue(json.loads((self.root / "release-scan-result.json").read_text())["passed"])

    def test_attestation_must_match_attached_predicate(self):
        (self.root / "wendy-release.spdx.json").write_text('{"name":"expected"}')
        artifact = self.root / "artifact"
        artifact.write_bytes(b"payload")
        for predicate, succeeds in (({"name": "expected"}, True), ({"name": "wrong"}, False)):
            result = subprocess.CompletedProcess([], 0, json.dumps([
                {"verificationResult": {"statement": {"predicate": predicate}}}]))
            with patch.object(security, "release_files", return_value=[artifact]), patch.object(security, "run", return_value=result):
                if succeeds:
                    security.verify(self.root, self.root, "owner/repo", "bundle")
                else:
                    with self.assertRaisesRegex(ValueError, "differs"):
                        security.verify(self.root, self.root, "owner/repo", "bundle")

    def test_native_inventory_binds_libraries_to_binary(self):
        binary = self.root / "wendy"
        binary.write_bytes(b"binary")
        doc = native.inventory(binary, ["libusb=1.0.29", "musl=1.2.5"])
        self.assertEqual(doc["files"][0]["checksums"][0]["checksumValue"], security.sha256(binary))
        self.assertEqual({p["name"] for p in doc["packages"]}, {"libusb", "musl"})
        with self.assertRaises(ValueError):
            native.inventory(binary, ["libusb="])

    def test_wasm_imports_deduplicate_and_follow_replacements(self):
        package = {"Module": {"Path": "original", "Replace": {"Path": "fork", "Version": "v1.0.0", "Sum": "h1:abc"}}}
        modules = wasm.modules_from_packages(json.dumps(package) + "\n" + json.dumps(package))
        self.assertEqual(list(modules), ["fork"])
        with self.assertRaisesRegex(ValueError, "unversioned"):
            wasm.modules_from_packages(json.dumps({"Module": {"Path": "local"}}))


if __name__ == "__main__":
    unittest.main()
