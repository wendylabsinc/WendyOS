"""Saved-receipt tests use fake signatures, never an authority/clock fixture."""
import base64
import copy
import json
import unittest

import history as h
import history_receipts as check
import receipts as r
from stage import APP, Refused
from test_history import inputs
from test_receipts import b64, raw, wrap


def fixture():
    commission, fusion, target = inputs()
    prepared = h.prepare(raw(commission), raw(fusion), raw(wrap(fusion)["exit.json"]), raw(target))
    config, _ = h.validate_prepared(prepared)
    expiry = 0
    for expected in config["expected"][:-1]:
        service = expected["reference"]["key"]["source"]["service"]
        item = h.decode_proof(r.binary(expected["envelope_json"], r.MAX_PROOF), service, commission)
        expiry = max(expiry, item["payload"]["captured_unix_ns"]+check.MAX_PHYSICAL_AGE_NS)
    earliest, latest = expiry+1, expiry+1_000_001
    retained = latest//1_000_000+60000
    result = dict(version=1, status="complete", physical_capture_expired=True,
                  original_versions_absent=True, current_source_authority=False,
                  final_physical_earliest_unix_ns=earliest, final_physical_latest_unix_ns=latest,
                  proofs=[dict(**copy.deepcopy(row), retain_until_unix_ms=retained, historical=True)
                          for row in config["expected"]],
                  media=[dict(**copy.deepcopy(row), retain_until_unix_ms=retained) for row in fusion["media"]])
    scene = dict(version=1, kind="wendy.agentmultimodal.scene.v1", scene_url="http://10.89.1.2:8091",
                 site=commission["site"], app_id=APP, run_id=commission["run_id"],
                 boot_id=commission["scene_boot_id"], package_sha256=commission["package_sha256"],
                 frame=commission["frame"], anchor_unix_ns=commission["anchor_unix_ns"],
                 clock_domain="simulation:"+commission["run_id"], simulation_time_ns=0,
                 observed_unix_ns=commission["anchor_unix_ns"], observation_sequence=1,
                 task=dict(container=APP+"_scene", app=APP, service="scene", pid=101,
                           start_ticks=200, netns_inode=300, cgroup="0::/system.slice:edge-agent:"+APP+"@scene",
                           ipv4="10.89.1.2", world_socket=False),
                 app_subnet="10.89.1.0/28")
    task = dict(container=APP+"_proof-reader", app=APP, service="proof-reader", pid=102,
                start_ticks=201, netns_inode=301,
                cgroup="0::/system.slice:edge-agent:"+APP+"@proof-reader.world-"+format(config["expected_generation"], "x"),
                ipv4="10.89.1.3",
                world_socket=True, generation=config["expected_generation"],
                socket_directory="/run/world/service-unique")
    return prepared, result, task, scene


class HistoryReceiptsTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.base = fixture()

    def values(self):
        return copy.deepcopy(self.base)

    def validate(self, prepared=None, result=None, task=None, scene=None, *, exit_value=None,
                 stdout=None, stderr=b""):
        p, out, t, s = self.values()
        p = p if prepared is None else prepared
        out = out if result is None else result
        t = t if task is None else task
        s = s if scene is None else scene
        stdout = raw(out) if stdout is None else stdout
        wrapper = wrap(out)["exit.json"] if exit_value is None else exit_value
        if exit_value is None:
            wrapper.update(stdout_bytes=len(stdout), stderr_bytes=len(stderr))
        return check.validate_history_receipts(p, raw(wrapper), stdout, stderr, t, s)

    def test_complete_exact_originals_media_and_honest_qualification(self):
        prepared, result, task, scene = self.values()
        before = copy.deepcopy((prepared, result, task, scene))
        stderr = b"bounded original stderr\x00\xff"
        summary = self.validate(prepared, result, task, scene, stderr=stderr)
        self.assertEqual(before, (prepared, result, task, scene))
        self.assertEqual(summary["phase"], "before_restart")
        self.assertEqual(summary["run_id"], "run-a")
        self.assertGreater(summary["reader"]["expected_generation"], 1 << 63)
        self.assertEqual(summary["reader_task"], task)
        self.assertEqual([p["reference"] for p in summary["proofs"]], [p["reference"] for p in result["proofs"]])
        self.assertEqual([m["source"]["service"] for m in summary["media"]], ["camera-a", "sensorlink-bridge", "audio"])
        self.assertEqual(summary["media"][1]["reference"]["content_type"], "image/jpeg")
        self.assertEqual(summary["media_bytes"], sum(len(base64.b64decode(m["data"])) for m in result["media"]))
        self.assertEqual(summary["outputs"]["stderr.log"], dict(bytes=len(stderr), sha256=r.sha(stderr)))
        for key in ("saved_receipt_consistency", "reader_reported_historical_retrieval",
                    "reader_reported_physical_capture_expired", "reader_reported_original_versions_absent"):
            self.assertIs(summary[key], True)
        for key in ("current_source_authority", "independent_cryptographic_verification",
                    "independent_clock_authentication", "independent_live_query"):
            self.assertIs(summary[key], False)
        self.assertIn("no raw Query reply", summary["qualification"])
        self.assertIn("Prepared", summary["qualification"])
        json.dumps(summary, allow_nan=False)
        summary["reader_task"]["generation"] = 1
        summary["proofs"][0]["reference"]["version"]["sequence"] = 9
        self.assertEqual(before, (prepared, result, task, scene))

    def test_after_restart_keeps_same_originals_and_pins_new_reader(self):
        first, result, task, scene = self.values()
        c, f, target = inputs()
        target.update(phase="after_restart", expected_generation=task["generation"]+1)
        second = h.prepare(raw(c), raw(f), raw(wrap(f)["exit.json"]), raw(target), first)
        with self.assertRaises(Refused):
            self.validate(second, result, task, scene)
        task["generation"] += 1
        task.update(pid=103, start_ticks=202, netns_inode=302, socket_directory="/run/world/replacement")
        summary = self.validate(second, result, task, scene)
        self.assertEqual(summary["phase"], "after_restart")
        self.assertEqual(summary["proof_bundle_sha256"], self.validate()["proof_bundle_sha256"])

    def test_missing_duplicate_reordered_or_changed_originals_refused(self):
        changes = [lambda o: o["proofs"].pop(), lambda o: o["proofs"].append(copy.deepcopy(o["proofs"][0])),
                   lambda o: o["proofs"].__setitem__(1, copy.deepcopy(o["proofs"][0])),
                   lambda o: o["proofs"].reverse(),
                   lambda o: o["proofs"][0]["reference"]["version"].update(sequence=101),
                   lambda o: o["proofs"][0]["reference"]["key"]["source"].update(service="gas"),
                   lambda o: o["proofs"][0]["reference"].update(record_sha256="f"*64),
                   lambda o: o["proofs"][0].update(historical=False),
                   lambda o: o["proofs"][0].update(historical=1),
                   lambda o: o["proofs"][0].update(envelope_json=b64(b"{}")),
                   lambda o: o["proofs"][0].update(unknown=1)]
        for i, change in enumerate(changes):
            with self.subTest(case=i):
                _, out, _, _ = self.values()
                change(out)
                with self.assertRaises(Refused): self.validate(result=out)
        _, out, _, _ = self.values()
        # Same decoded proof with whitespace and a recomputed outer hash still
        # differs from the commissioned original bytes.
        proof = out["proofs"][0]
        rewritten = b"\n"+base64.b64decode(proof["envelope_json"])
        proof.update(envelope_json=b64(rewritten))
        proof["reference"]["envelope_sha256"] = r.sha(rewritten)
        with self.assertRaises(Refused): self.validate(result=out)

    def test_media_must_use_original_owner_delivered_format_and_bytes(self):
        changes = [lambda o: o["media"].pop(), lambda o: o["media"].reverse(),
                   lambda o: o["media"].__setitem__(1, copy.deepcopy(o["media"][0])),
                   lambda o: o["media"][0]["source"].update(service="gas"),
                   lambda o: o["media"][0]["source"].update(app="wrong"),
                   lambda o: o["media"][0]["source"].update(stream="camera-b"),
                   lambda o: o["media"][1]["reference"].update(content_type="image/png"),
                   lambda o: o["media"][0]["reference"].update(size=True),
                   lambda o: o["media"][0].update(data=b64(b"changed bytes")),
                   lambda o: o["media"][0].update(unknown=1)]
        for i, change in enumerate(changes):
            with self.subTest(case=i):
                _, out, _, _ = self.values(); change(out)
                with self.assertRaises(Refused): self.validate(result=out)
        _, out, _, _ = self.values()
        changed = b"\xff\xd8changed\xff\xd9"
        out["media"][1].update(data=b64(changed), reference=dict(content_type="image/jpeg", size=len(changed), sha256=r.sha(changed)))
        with self.assertRaises(Refused): self.validate(result=out)

    def test_wrapper_exact_byte_counts_and_failure_outputs(self):
        _, out, _, _ = self.values()
        for key, value in (("version", True), ("exit_code", False), ("exit_code", 1), ("termination", "timeout"),
                           ("succeeded", False), ("output_truncated", True), ("child_started", False),
                           ("signal", 15), ("signal", False), ("forwarded_signal", 2),
                           ("stdout_bytes", 1), ("stderr_bytes", 1), ("unknown", 1)):
            w = wrap(out)["exit.json"]; w[key] = value
            original = raw(w), raw(out), b""
            with self.subTest(key=key, value=value), self.assertRaises(Refused):
                self.validate(result=out, exit_value=w)
            self.assertEqual(original, (raw(w), raw(out), b""))
        self.validate(stdout=b"\n"+raw(out)+b"\n", stderr=b"not necessarily UTF8\xff")
        w = wrap(out)["exit.json"]
        with self.assertRaises(Refused): self.validate(exit_value=w, stdout=raw(out)+b"\n")
        with self.assertRaises(Refused): self.validate(stderr=b"x"*(check.MAX_STDERR+1))

    def test_status_flags_are_required_not_inferred_from_partial_receipts(self):
        for key, value in (("version", True), ("status", "failed"), ("status", "unknown_outcome"),
                           ("physical_capture_expired", False), ("physical_capture_expired", 1),
                           ("original_versions_absent", False), ("original_versions_absent", 1),
                           ("current_source_authority", True), ("current_source_authority", 0),
                           ("error", "live_query_refused"), ("run_id", "run-a")):
            _, out, _, _ = self.values(); out[key] = value
            with self.subTest(key=key, value=value), self.assertRaises(Refused): self.validate(result=out)
        for key in self.base[1]:
            _, out, _, _ = self.values(); del out[key]
            with self.subTest(missing=key), self.assertRaises(Refused): self.validate(result=out)

    def test_reported_clock_must_prove_capture_expiry_and_all_retention(self):
        summary = self.validate()
        expired = summary["original_capture_expired_after_unix_ns"]
        _, out, _, _ = self.values()
        for earliest, latest in ((expired, expired+1), (expired-1, expired), (expired+2, expired+1),
                                 (expired+1, expired+1+5_000_000_001), (True, expired+1), (1 << 63, 1 << 63)):
            changed = copy.deepcopy(out)
            changed.update(final_physical_earliest_unix_ns=earliest, final_physical_latest_unix_ns=latest)
            with self.subTest(interval=(earliest, latest)), self.assertRaises(Refused): self.validate(result=changed)
        # Equality at the retention boundary is expired for every proof/media.
        for group in ("proofs", "media"):
            for index in range(len(out[group])):
                for until in (0, -1, True, (1 << 63), out["final_physical_latest_unix_ns"]//1_000_000):
                    changed = copy.deepcopy(out); changed[group][index]["retain_until_unix_ms"] = until
                    with self.subTest(group=group, index=index, until=until), self.assertRaises(Refused):
                        self.validate(result=changed)
        exact = copy.deepcopy(out)
        until = (out["final_physical_latest_unix_ns"]+999999)//1_000_000
        exact["proofs"][0]["retain_until_unix_ms"] = until
        exact["final_physical_latest_unix_ns"] = until*1_000_000
        with self.assertRaises(Refused): self.validate(result=exact)

    def test_independent_reader_task_cannot_be_replaced_by_receipt_hashes(self):
        changes = {"generation": 1, "container": APP+"_fusion", "app": "wrong", "service": "fusion",
                   "world_socket": False, "pid": True, "start_ticks": 0, "netns_inode": 300,
                   "cgroup": self.base[3]["task"]["cgroup"], "socket_directory": "/run/../world", "ipv4": "10.89.1.2",
                   "unknown": 1}
        for key, value in changes.items():
            _, _, task, _ = self.values(); task[key] = value
            with self.subTest(key=key), self.assertRaises(Refused): self.validate(task=task)
        for generation in (True, 0, 1 << 64):
            _, _, task, _ = self.values(); task["generation"] = generation
            with self.subTest(generation=generation), self.assertRaises(Refused): self.validate(task=task)
        prepared, _, _, _ = self.values()
        c, p = json.loads(prepared.config), json.loads(prepared.receipt)
        c["expected_generation"] += 1
        p["reader"]["expected_generation"] += 1
        p["config_sha256"] = r.sha(raw(c))
        # Every local Prepared hash matches, but the independent task does not.
        changed = h.Prepared(raw(c), raw(p))
        h.validate_prepared(changed)
        with self.assertRaises(Refused): self.validate(prepared=changed)

    def test_observed_unified_cgroup_lines_preserved_and_malformed_lines_refused(self):
        # Run F observations contain literal stripped /proc/PID/cgroup lines.
        # Adapt its @fusion.world-<generation> form to the future reader service.
        _, _, task, scene = self.values()
        scene["task"]["cgroup"] = "0::/system.slice:edge-agent:"+APP+"@scene"
        task["cgroup"] = "0::/system.slice:edge-agent:"+APP+"@proof-reader.world-"+format(task["generation"], "x")
        got = self.validate(task=task, scene=scene)
        self.assertEqual(got["reader_task"]["cgroup"], task["cgroup"])
        self.assertEqual(got["scene_sha256"], r.sha(h.encode(scene)))
        invalid = ("", "/system.slice/reader", "1::/system.slice/reader", "0:cpu:/system.slice/reader",
                   "0::", "0::relative", "0::/", "0:://a", "0::/a/../b", "0::/a/./b", "0::/a//b",
                   "0::/a\n0::/b", "0::/a\n", "0::/a\r0::/b", "0::/a\x00b", " 0::/a",
                   "0::/a ", "0::/"+"x"*4096)
        for value in invalid:
            for target in ("reader", "scene"):
                _, _, t, s = self.values()
                (t if target == "reader" else s["task"])["cgroup"] = value
                with self.subTest(target=target, cgroup=value[:80]), self.assertRaises(Refused):
                    self.validate(task=t, scene=s)

    def test_scene_binds_original_run_boot_frame_package_anchor(self):
        changes = {"run_id": "another-run", "site": "elsewhere", "app_id": "wrong", "boot_id": "new-boot",
                   "package_sha256": "f"*64, "frame": dict(id="floor", revision="r2"), "anchor_unix_ns": 1,
                   "clock_domain": "simulation:another-run", "scene_url": "http://10.89.1.3:8091",
                   "app_subnet": "10.89.2.0/28", "version": True, "simulation_time_ns": 1 << 63,
                   "observation_sequence": 0, "observed_unix_ns": True, "unknown": 1}
        for key, value in changes.items():
            _, _, _, scene = self.values(); scene[key] = value
            with self.subTest(key=key), self.assertRaises(Refused): self.validate(scene=scene)
        for key, value in (("world_socket", True), ("container", APP+"_proof-reader"), ("ipv4", "10.89.1.16")):
            _, _, _, scene = self.values(); scene["task"][key] = value
            with self.subTest(scene_task=key), self.assertRaises(Refused): self.validate(scene=scene)

    def test_strict_raw_json_and_bounds(self):
        prepared, out, task, scene = self.values()
        exit_bytes = raw(wrap(out)["exit.json"])
        for bad in (b'{"version":1,"version":1}', b'{"version":NaN}', b'{"version":"\\ud800"}',
                    b"\xff", b"["*100+b"]"*100, b" "*(r.MAX_APP+1)):
            with self.subTest(stdout=bad[:32]), self.assertRaises(Refused):
                check.validate_history_receipts(prepared, exit_bytes, bad, b"", task, scene)
        for bad in (b'{"version":1,"version":1}', b" "*(check.MAX_EXIT+1), b""):
            with self.subTest(exit=bad[:32]), self.assertRaises(Refused):
                check.validate_history_receipts(prepared, bad, raw(out), b"", task, scene)
        for field in ("proofs", "media"):
            changed = copy.deepcopy(out); changed[field] = None
            with self.subTest(field=field), self.assertRaises(Refused): self.validate(result=changed)
        with self.assertRaises(Refused): self.validate(stderr="not bytes")
        with self.assertRaises(Refused): self.validate(task={"self": []})


if __name__ == "__main__":
    unittest.main()
