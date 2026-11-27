"""Preparation tests use fake signature bytes, never a trust/authority fixture."""
import base64
import copy
import json
from pathlib import Path
import subprocess
import tempfile
import unittest

import history as h
import receipts as r
from stage import APP, Refused
from test_receipts import fixture, raw, b64, sign, wrap


def inputs():
    plan, scene, states, collected = fixture()
    f = collected["fusion"]["stdout.json"]
    # Fill the complete real Hazard JSON shape, beyond the older validator's
    # intentionally minimal structural fixture. All signatures remain fake.
    hazard = f["evaluation"]["hazard"]
    hazard.update(confidence=0.8, confidence_meaning="synthetic test score", position_m=[0, 0, 0],
                  uncertainty_m=0.1, location_basis="fixture gas sensor", gas_value_ppm=80,
                  gas_uncertainty_ppm=1, gas_species="test",
                  cameras=[dict(sensor_id=x, red_fraction=0.8) for x in ("camera-a", "camera-b")],
                  audio=dict(rms=0.1, tone_fraction=0.8))
    signed = json.loads(base64.b64decode(f["signed_record_json"]))
    record = json.loads(base64.b64decode(signed["record"]))
    p = record["publication"]
    result = json.loads(base64.b64decode(p["payload"]))
    result["result_json"] = b64(raw(hazard))
    p["payload"] = b64(raw(result))
    f.update(sign(plan, states, "fusion", p, 1))
    c = {key: plan[key] for key in ("tenant", "site", "device", "app", "run_id", "frame",
                                    "package_sha256", "anchor_unix_ns", "scenario_version", "seed")}
    c.update(version=1, scene_boot_id=scene["boot_id"],
             publisher_generations={s: states[s]["generation"] for s in h.ORDER})
    target = dict(phase="before_restart", app=APP, service="proof-reader",
                  expected_generation=(1 << 63)+201, duration_ms=45000)
    return c, f, target


class HistoryTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.fixture = inputs()

    def values(self):
        return copy.deepcopy(self.fixture)

    def prepare(self, c=None, f=None, target=None, previous=None, exit_value=None):
        default = self.values()
        c = default[0] if c is None else c
        f = default[1] if f is None else f
        target = default[2] if target is None else target
        stdout = raw(f)
        exit_value = wrap(f)["exit.json"] if exit_value is None else exit_value
        return h.prepare(raw(c), stdout, raw(exit_value), raw(target), previous)

    def test_exact_five_original_envelopes_and_bound_media(self):
        c, f, target = self.values()
        prepared = self.prepare(c, f, target)
        config, receipt = h.validate_prepared(prepared)
        by_service = {x["reference"]["key"]["source"]["service"]: x for x in f["inputs"]}
        self.assertEqual([x["reference"]["key"]["source"]["service"] for x in config["expected"]], list(h.ORDER))
        for x in config["expected"][:-1]:
            self.assertEqual(x, by_service[x["reference"]["key"]["source"]["service"]])
        self.assertEqual(config["expected"][-1]["envelope_json"], f["signed_record_json"])
        self.assertEqual(config["expected"][-1]["reference"]["envelope_sha256"], f["proof_sha256"])
        self.assertEqual(config["expected_generation"], target["expected_generation"])
        self.assertEqual([x["source"]["service"] for x in receipt["media"]], ["camera-a", "sensorlink-bridge", "audio"])
        bridge = receipt["media"][1]
        self.assertEqual(bridge["reference"]["content_type"], "image/jpeg")
        self.assertEqual(bridge["original_evidence"]["content_type"], "image/png")
        self.assertNotEqual(bridge["reference"], bridge["original_evidence"])
        self.assertNotIn("media", config)
        self.assertFalse(receipt["retrieval_verified"])
        self.assertFalse(receipt["current_source_authority"])

    def test_second_generation_retains_original_order_and_bytes(self):
        first = self.prepare()
        c, f, target = self.values()
        target.update(phase="after_restart", expected_generation=target["expected_generation"]+1)
        second = self.prepare(c, f, target, first)
        a, ap = h.validate_prepared(first)
        b, bp = h.validate_prepared(second)
        self.assertEqual(a["expected"], b["expected"])
        self.assertEqual(ap["references"], bp["references"])
        self.assertEqual(ap["proof_bundle_sha256"], bp["proof_bundle_sha256"])
        self.assertNotEqual(a["expected_generation"], b["expected_generation"])
        with self.assertRaises(Refused): h.validate_prepared(h.Prepared(second.config, first.receipt))
        with self.assertRaises(Refused): h.validate_prepared(h.Prepared(first.config, second.receipt))

    def test_phase_order_and_old_reader_generation_refused(self):
        first = self.prepare()
        c, f, target = self.values()
        with self.assertRaises(Refused): self.prepare(c, f, target, first)
        target["phase"] = "after_restart"
        with self.assertRaises(Refused): self.prepare(c, f, target)
        with self.assertRaises(Refused): self.prepare(c, f, target, first)
        target["expected_generation"] += 1
        target["service"] = "fusion"
        with self.assertRaises(Refused): self.prepare(c, f, target, first)

    def test_saved_phase_cannot_substitute_an_unbound_media_list(self):
        prepared = self.prepare()
        receipt = json.loads(prepared.receipt)
        receipt["media"][1]["reference"]["sha256"] = "e"*64
        with self.assertRaises(Refused): h.validate_prepared(h.Prepared(prepared.config, raw(receipt)))
        for key in ("unknown", "expected_generation"):
            c, p = json.loads(prepared.config), json.loads(prepared.receipt)
            if key == "unknown": c["expected"][0]["unknown"] = 1
            else: c[key] = True
            p["config_sha256"] = r.sha(raw(c))
            p["proof_bundle_sha256"] = r.sha(h.encode(c["expected"]))
            with self.subTest(key=key), self.assertRaises(Refused):
                h.validate_prepared(h.Prepared(raw(c), raw(p)))

    def test_tampered_refs_duplicate_sources_and_incomplete_proofs(self):
        changes = [lambda f: f["inputs"][0]["reference"]["version"].update(sequence=999),
                   lambda f: f["inputs"][0]["reference"].update(envelope_sha256="e"*64),
                   lambda f: f["inputs"][0].update(envelope_json=b64(b"{}")),
                   lambda f: f["inputs"].__setitem__(1, copy.deepcopy(f["inputs"][0])),
                   lambda f: f["inputs"].pop(), lambda f: f.update(proof_sha256="e"*64),
                   lambda f: f.update(record_sha256="e"*64), lambda f: f.pop("signed_record_json")]
        for i, change in enumerate(changes):
            with self.subTest(case=i):
                c, f, t = self.values(); change(f)
                with self.assertRaises(Refused): self.prepare(c, f, t)

    def test_wrong_commission_run_scope_generation_or_scene(self):
        for key, value in (("tenant", "wrong"), ("site", "wrong"), ("device", "wrong"),
                           ("run_id", "wrong"), ("scene_boot_id", "new-boot"),
                           ("package_sha256", "b"*64), ("anchor_unix_ns", 1)):
            with self.subTest(key=key):
                c, f, t = self.values(); c[key] = value
                with self.assertRaises(Refused): self.prepare(c, f, t)
        for service in h.ORDER:
            c, f, t = self.values(); c["publisher_generations"][service] += 1
            with self.subTest(service=service), self.assertRaises(Refused): self.prepare(c, f, t)

    def test_success_claim_wrapper_and_media_must_all_match(self):
        changes = [lambda f: f.update(status="unknown_outcome"), lambda f: f.update(status="failed"),
                   lambda f: f.update(locally_verified=False), lambda f: f.pop("evaluation"),
                   lambda f: f["media"].pop(), lambda f: f["media"][0]["source"].update(service="gas"),
                   lambda f: f["media"][0].update(data=b64(b"wrong bytes")),
                   lambda f: f["media"][0]["reference"].update(sha256="f"*64)]
        for i, change in enumerate(changes):
            with self.subTest(case=i):
                c, f, t = self.values(); change(f)
                with self.assertRaises(Refused): self.prepare(c, f, t)
        for key, value in (("stdout_bytes", 1), ("termination", "timeout"), ("succeeded", False),
                           ("output_truncated", True), ("exit_code", 1)):
            c, f, t = self.values(); exit_value = wrap(f)["exit.json"]; exit_value[key] = value
            with self.subTest(key=key), self.assertRaises(Refused): self.prepare(c, f, t, exit_value=exit_value)

    def test_duplicate_unknown_and_bounded_json(self):
        c, f, t = self.values()
        valid = [raw(c), raw(f), raw(wrap(f)["exit.json"]), raw(t)]
        for i in range(4):
            for bad in (b'{"version":1,"version":1}', b'{"version":NaN}', b" "*(h.MAX_INPUT+1)):
                with self.subTest(input=i, kind=bad[:25]):
                    values = list(valid); values[i] = bad
                    with self.assertRaises(Refused): h.prepare(*values)
        for i in range(4):
            values = list(valid); value = json.loads(values[i]); value["unknown"] = 1; values[i] = raw(value)
            with self.subTest(unknown=i), self.assertRaises(Refused): h.prepare(*values)
        f["evaluation"]["hazard"]["audio"]["unknown"] = 1
        with self.assertRaises(Refused): self.prepare(c, f, t)

    def test_changed_fusion_bytes_refused_in_later_phase(self):
        first = self.prepare()
        c, f, target = self.values()
        target.update(phase="after_restart", expected_generation=target["expected_generation"]+1)
        f["attempts"] += 1
        with self.assertRaises(Refused): self.prepare(c, f, target, first)

    def test_nested_unknown_capture_fields_refused(self):
        c, f, _ = self.values()
        entry = f["inputs"][0]
        item = h.decode_proof(r.binary(entry["envelope_json"], r.MAX_PROOF), "camera-a", c)
        sample = json.loads(base64.b64decode(item["payload"]["sample_json"]))
        sample["camera"]["unknown"] = 1
        item["payload"]["sample_json"] = b64(raw(sample))
        with self.assertRaises(Refused): h.capture(item, "camera-a", c)

    def test_large_config_uses_stdin_exactly_and_refuses_overwrite(self):
        c, f, t = self.values()
        padded = b"\n"*(120 << 10) + base64.b64decode(f["signed_record_json"])
        f.update(signed_record_json=b64(padded), proof_sha256=r.sha(padded))
        prepared = self.prepare(c, f, t)
        self.assertGreater(len(prepared.config), 128 << 10)
        transfer = h.stdin_transfer(prepared, "/var/lib/wendy/volumes/history-before/config.json")
        self.assertEqual(transfer.stdin, prepared.config)
        self.assertLess(max(map(len, transfer.argv)), 16 << 10)
        with tempfile.TemporaryDirectory(dir="/private/tmp") as directory:
            dest = Path(directory) / "config.json"
            argv = list(transfer.argv); argv[3] = str(dest)
            result = subprocess.run(argv, input=transfer.stdin, capture_output=True, timeout=12)
            self.assertEqual(result.returncode, 0, result.stderr.decode())
            self.assertEqual(dest.read_bytes(), prepared.config)
            self.assertEqual(dest.stat().st_mode & 0o777, 0o600)
            result = subprocess.run(argv, input=transfer.stdin, capture_output=True, timeout=12)
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual(dest.read_bytes(), prepared.config)

    def test_stdin_receiver_refuses_truncation_extra_bytes_and_symlink(self):
        prepared = self.prepare()
        transfer = h.stdin_transfer(prepared, "/var/lib/wendy/volumes/history-before/config.json")
        with tempfile.TemporaryDirectory(dir="/private/tmp") as directory:
            root = Path(directory); dest = root / "config.json"
            argv = list(transfer.argv); argv[3] = str(dest)
            for data in (transfer.stdin[:-1], transfer.stdin+b" ", b"x"*len(transfer.stdin)):
                result = subprocess.run(argv, input=data, capture_output=True, timeout=12)
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(dest.exists())
            actual = root / "actual"; actual.mkdir()
            link = root / "link"; link.symlink_to(actual, target_is_directory=True)
            argv[3] = str(link / "config.json")
            result = subprocess.run(argv, input=transfer.stdin, capture_output=True, timeout=12)
            self.assertNotEqual(result.returncode, 0)
            self.assertFalse((actual / "config.json").exists())
        for path in ("/tmp/config.json", "/var/lib/wendy/volumes/x/../config.json", "/var/lib/wendy/volumes/x/token"):
            with self.subTest(path=path), self.assertRaises(Refused): h.stdin_transfer(prepared, path)

    def test_phase_files_and_failed_outputs_are_never_replaced(self):
        prepared = self.prepare()
        with tempfile.TemporaryDirectory(dir="/private/tmp") as directory:
            phase = h.write_phase(directory, prepared)
            self.assertEqual((phase / "config.json").read_bytes(), prepared.config)
            self.assertEqual((phase / "preparation.json").read_bytes(), prepared.receipt)
            with self.assertRaises(FileExistsError): h.write_phase(directory, prepared)
            h.retain_output(phase, b'{"succeeded":false}', b"", b"original failure\n")
            self.assertEqual((phase / "reader-stdout.json").read_bytes(), b"")
            before = (phase / "reader-stderr.log").read_bytes()
            with self.assertRaises(Refused): h.retain_output(phase, b"{}", b"{}", b"replacement")
            self.assertEqual((phase / "reader-stderr.log").read_bytes(), before)
            manifest = json.loads((phase / "output-manifest.json").read_bytes())
            self.assertFalse(manifest["retrieval_verified"])
            self.assertEqual(manifest["files"]["reader-stdout.json"]["size"], 0)

    def test_local_phase_path_symlink_is_refused(self):
        prepared = self.prepare()
        with tempfile.TemporaryDirectory(dir="/private/tmp") as directory:
            actual = Path(directory) / "actual"; actual.mkdir()
            link = Path(directory) / "link"; link.symlink_to(actual, target_is_directory=True)
            with self.assertRaises(OSError): h.write_phase(link, prepared)
            self.assertEqual(list(actual.iterdir()), [])


if __name__ == "__main__":
    unittest.main()
