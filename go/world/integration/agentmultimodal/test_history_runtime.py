"""History orchestration tests: fake Runner only, no Docker or app execution."""
import copy
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import configs
import history as h
import history_runtime as runtime
import history_stage as bundle_stage
import receipts as r
import stage
from stage import Refused
from test_history import inputs
from test_history_receipts import fixture as history_fixture
from test_receipts import raw, wrap


class FakeRunner:
    def __init__(self, root):
        self.out = root/"out"; self.out.mkdir(mode=0o700)
        self.source = root/"baseline"; self.source.mkdir(mode=0o700)
        self.name = "owned-runtime"
        self.agent_pid = 500
        self.c, self.fusion, _ = inputs()
        _, self.reader_output, self.reader_task, self.scene = history_fixture()
        self.plan = {key: self.c[key] for key in ("app", "site", "run_id", "frame", "package_sha256",
                                                 "anchor_unix_ns", "scenario_version", "seed")}
        self.public = dict(tenant=self.c["tenant"], principal=self.c["device"])
        self.states = {service: dict(generation=generation, socket_directory="/run/world/"+service)
                       for service, generation in self.c["publisher_generations"].items()}
        self.calls, self.remote, self.configs = [], {}, {}
        self.phase = None
        self.ordinal = 0
        self.fail = None
        self.alter = None
        self.clock = None
        self.current_manifest = "a"*64

    def result(self, value=b"", code=0):
        return subprocess.CompletedProcess([], code, value, b"")

    def record(self, operation, **fields):
        self.calls.append(dict(operation=operation, **fields))
        if self.fail == operation:
            raise Refused("injected "+operation)

    def resource_state(self, *args):
        self.record("ownership")
        return "foreign" if self.alter == "foreign" else "owned"

    def observe_scene(self):
        self.record("scene")
        self.scene = copy.deepcopy(self.scene)
        self.scene["observation_sequence"] += 1
        self.scene["observed_unix_ns"] += 1
        self.scene["simulation_time_ns"] += 1
        if self.alter == "scene_restart" and self.agent_pid != 500:
            self.scene["boot_id"] = "changed-boot"
        return copy.deepcopy(self.scene)

    def python(self, script, *args, **kwargs):
        operation = script.splitlines()[0].removeprefix("# history:")
        self.record(operation, args=args, timeout=kwargs.get("timeout"))
        if operation == "reader-task":
            task = self.task()
            return self.result(raw({k: task[k] for k in ("pid", "start_ticks", "netns_inode", "cgroup")}))
        if operation == "reader-done" and self.alter == "wait_timeout":
            self.clock[0] += 61
            return self.result(code=1)
        return self.result(b"okay")

    def call(self, argv, **kwargs):
        script, args, data = argv[6], argv[7:], kwargs["input_data"]
        self.assert_bound(data, kwargs["timeout"])
        if script == runtime.WRITE_CHUNK:
            self.record("upload", args=args, bytes=len(data))
            path, offset, total, digest, _ = args
            offset, total = int(offset), int(total)
            existing = self.remote.get(path, b"")
            if len(existing) != offset:
                raise Refused("wrong transfer offset")
            self.remote[path] = existing+data
            if len(self.remote[path]) == total and r.sha(self.remote[path]) != digest:
                raise Refused("wrong transfer digest")
        elif script == runtime.INSTALL_MANIFEST:
            self.record("manifest", args=args)
            _, previous, wanted, phase = args
            if previous != self.current_manifest or r.sha(data) != wanted or self.alter == "wrong_manifest":
                raise Refused("manifest transition differs")
            self.current_manifest, self.phase = wanted, phase
        elif script == h.STDIN_RECEIVER:
            self.record("config", args=args)
            _, digest, length = args
            if r.sha(data) != digest or len(data) != int(length):
                raise Refused("config bytes differ")
            self.configs[self.phase] = json.loads(data)
        else:
            raise AssertionError("unapproved command")
        return self.result(b"accepted")

    @staticmethod
    def assert_bound(data, timeout):
        assert 0 < len(data) <= 1 << 20 and 0 < timeout <= 15

    def cli(self, *args, **kwargs):
        self.record("cli", args=args, timeout=kwargs["timeout"])
        if args[-2:] != ("--service", h.READER):
            raise AssertionError("baseline selected")
        self.ordinal += 1
        return self.result()

    def task(self):
        task = copy.deepcopy(self.reader_task)
        task.update(pid=102+self.ordinal, start_ticks=201+self.ordinal, netns_inode=301+self.ordinal,
                    generation=task["generation"]+self.ordinal, socket_directory="/run/world/reader-"+str(self.ordinal))
        task["cgroup"] = "0::/system.slice:edge-agent:"+stage.APP+"@proof-reader.world-"+format(task["generation"], "x")
        if self.alter == "same_generation" and self.ordinal == 2:
            task["generation"] -= 1
        if self.alter == "same_socket" and self.ordinal == 2:
            task["socket_directory"] = "/run/world/reader-1"
        if self.alter == "baseline_socket":
            task["socket_directory"] = self.states["fusion"]["socket_directory"]
        return task

    def inside(self, *args, **kwargs):
        self.record("inside", args=args, timeout=kwargs["timeout"])
        task = self.task()
        if args[:3] == ("ctr", "containers", "info"):
            labels = {"sh.wendy/app.id": stage.APP, "sh.wendy/service": h.READER,
                      "sh.wendy/world.deployment-generation": str(task["generation"]),
                      "sh.wendy/world.socket-directory": task["socket_directory"]}
            if self.alter == "wrong_service": labels["sh.wendy/service"] = "fusion"
            return self.result(raw(dict(Labels=labels)))
        if args == ("ctr", "tasks", "list"):
            return self.result(("TASK PID STATUS\n"+stage.APP+"_proof-reader "+str(task["pid"])+" RUNNING\n").encode())
        if args[0] == "nsenter":
            return self.result(raw([dict(addr_info=[dict(family="inet", local=task["ipv4"])])]))
        raise AssertionError(args)

    def snapshot_file(self, source, destination, limit):
        self.record("snapshot", source=source, destination=str(destination), limit=limit)
        phase = "after_restart" if "after-restart" in source else "before_restart"
        out = copy.deepcopy(self.reader_output)
        if phase in self.configs:
            for proof, expected in zip(out["proofs"], self.configs[phase]["expected"]):
                proof.update(copy.deepcopy(expected))
        else:
            out.update(status="failed")
        if self.alter == "reader_failed": out.update(status="failed")
        if self.alter == "retention_changed" and phase == "after_restart":
            out["media"][0]["retain_until_unix_ms"] += 1
        stdout, stderr = raw(out), b"original bounded stderr\x00\xff"
        wrapper = wrap(out)["exit.json"]
        wrapper.update(stdout_bytes=len(stdout), stderr_bytes=len(stderr))
        content = {"exit.json": raw(wrapper), "stdout.json": stdout, "stderr.log": stderr}[Path(source).name]
        stage.write_new(destination, content)
        return dict(status="captured", size=len(content), sha256=r.sha(content))

    def stop_agent(self):
        self.record("stop_agent")

    def start_agent(self, phase):
        self.record("start_agent", phase=phase)
        if self.alter != "same_agent": self.agent_pid += 1

    def finish(self):
        self.record("global_cleanup")


class HistoryRuntimeTests(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory(prefix="world-history-runtime-", dir="/private/tmp")
        self.addCleanup(temp.cleanup)
        self.root = Path(temp.name).resolve(); self.root.chmod(0o700)
        self.runner = FakeRunner(self.root)
        self.bundle = self.root/"bundle"; self.bundle.mkdir(mode=0o700)
        binary = b"fixture app bytes"
        context = self.bundle/bundle_stage.CONTEXT; context.mkdir(parents=True)
        (context/"app").write_bytes(binary)
        (context/"app-runner").write_bytes(binary+b" runner")
        (context/"Dockerfile").write_bytes(stage.dockerfile(h.READER, self.runner.plan["run_id"],
                    self.runner.plan["anchor_unix_ns"], self.runner.plan["seed"], {}))
        previous = self.runner.current_manifest
        phases = []
        for name in h.PHASES:
            path = bundle_stage.MANIFESTS[name]
            data = raw(dict(phase=name, fixture=True))
            (self.bundle/path).parent.mkdir(parents=True, exist_ok=True)
            (self.bundle/path).write_bytes(data)
            phases.append(dict(name=name, manifest=path, manifest_sha256=r.sha(data),
                               expected_previous_manifest_sha256=previous,
                               volumes=bundle_stage.phase_volumes(self.runner.plan["run_id"], name)))
            previous = r.sha(data)
        self.plan = dict(run_id=self.runner.plan["run_id"], phases=phases,
                         binaries={h.READER: dict(sha256=r.sha(binary), size=len(binary)),
                                   "app-runner": dict(sha256=r.sha(binary+b" runner"), size=len(binary+b" runner"))})
        patcher = patch.object(bundle_stage, "validate_bundle", side_effect=lambda *a, **k: copy.deepcopy(self.plan))
        patcher.start(); self.addCleanup(patcher.stop)
        self.hooks = runtime.HistoryRuntime(self.runner, self.bundle, reader_sha256="b"*64,
                                           baseline_plan_sha256="c"*64, baseline_inventory_sha256="d"*64)

    def prepare_hooks(self):
        self.hooks.commission(configs.commission_spec(self.runner.scene))
        values = {s: {} for s in stage.SERVICES if s != "scene"}
        values["fusion"]["retain_for_ms"] = 60000
        self.hooks.configurations(values)

    def execute(self, out=None, wrapper=None, commission=None):
        out = self.runner.fusion if out is None else out
        stdout = raw(out)
        wrapper = wrap(out)["exit.json"] if wrapper is None else wrapper
        return self.hooks.execute(commission or self.hooks.source_commission(), stdout, raw(wrapper))

    def operations(self):
        return [x["operation"] for x in self.runner.calls]

    def test_opt_in_hooks_are_pure_and_keep_baseline_unchanged(self):
        self.assertEqual(self.runner.calls, [])
        spec = configs.commission_spec(self.runner.scene)
        original = copy.deepcopy(spec)
        result = self.hooks.commission(spec)
        self.assertEqual(spec, original)
        self.assertEqual(result, {**original, "history_reader": True})
        values = {s: {} for s in stage.SERVICES if s != "scene"}
        values["fusion"]["retain_for_ms"] = 60000
        original = copy.deepcopy(values)
        changed = self.hooks.configurations(values)
        self.assertEqual(values, original)
        changed["fusion"]["retain_for_ms"] = 60000
        self.assertEqual(changed, original)
        self.assertEqual(self.runner.calls, [])
        self.assertFalse(self.hooks.directory.exists())

    def test_exact_success_two_phases_normal_cli_and_originals_preserved(self):
        self.prepare_hooks()
        result = self.execute()
        self.assertTrue(result["passed"])
        self.assertTrue(result["same_originals_across_restart"])
        self.assertFalse(result["current_source_authority"])
        a, b = result["phases"]
        self.assertEqual(a["proof_bundle_sha256"], b["proof_bundle_sha256"])
        self.assertEqual(a["proofs"], b["proofs"])
        self.assertEqual(a["media"], b["media"])
        self.assertNotEqual(a["reader"]["expected_generation"], b["reader"]["expected_generation"])
        self.assertEqual([p["phase"] for p in result["phases"]], list(h.PHASES))
        operations = self.operations()
        self.assertEqual(operations.count("cli"), 2)
        self.assertEqual(operations.count("stop_agent"), 1)
        self.assertEqual(operations.count("start_agent"), 1)
        self.assertEqual(operations.count("release-gate"), 2)
        self.assertLess(operations.index("config"), operations.index("release-gate"))
        self.assertLess(operations.index("snapshot"), operations.index("stop_agent"))
        self.assertNotIn("global_cleanup", operations)
        for phase in h.PHASES:
            directory = self.hooks.directory/phase
            config = json.loads((directory/"config.json").read_bytes())
            self.assertNotIn("roots_der", config)
            self.assertEqual(config["expected"], self.runner.configs[phase]["expected"])
            for name, _ in runtime.OUTPUTS:
                self.assertEqual((directory/"receipts"/name).read_bytes(), (directory/"final-outputs"/name).read_bytes())
        with self.assertRaises(Refused): self.execute()
        self.assertEqual(operations, self.operations())

    def test_failure_or_ambiguous_ack_refused_before_any_mutation(self):
        self.prepare_hooks()
        for change in (lambda o: o.update(status="failed"), lambda o: o.update(status="unknown_outcome"),
                       lambda o: o.update(locally_verified=False), lambda o: o.pop("signed_record_json"),
                       lambda o: o.update(proof_sha256="f"*64), lambda o: o["inputs"].pop()):
            with self.subTest(change=change):
                self.hooks._used = False
                out = copy.deepcopy(self.runner.fusion); change(out)
                with self.assertRaises(Refused): self.execute(out=out)
                self.assertEqual(self.runner.calls, [])
                self.assertFalse(self.hooks.directory.exists())
        self.hooks._used = False
        w = wrap(self.runner.fusion)["exit.json"]; w["stdout_bytes"] += 1
        with self.assertRaises(Refused): self.execute(wrapper=w)
        self.assertEqual(self.runner.calls, [])

    def test_commission_and_stdin_size_pins_checked_before_mutation(self):
        self.prepare_hooks()
        c = json.loads(self.hooks.source_commission()); c["publisher_generations"]["gas"] += 1
        with self.assertRaises(Refused): self.execute(commission=raw(c))
        self.assertEqual(self.runner.calls, [])
        self.hooks._used = False
        with patch.object(runtime, "MAX_STDIN", 1024), self.assertRaises(Refused): self.execute()
        self.assertEqual(self.runner.calls, [])
        self.assertFalse(self.hooks.directory.exists())

    def test_remote_manifest_refusal_never_launches_reader(self):
        self.prepare_hooks(); self.runner.alter = "wrong_manifest"
        with self.assertRaises(Refused): self.execute()
        self.assertNotIn("cli", self.operations())
        self.assertNotIn("release-gate", self.operations())
        self.assertTrue((self.hooks.directory/"before_restart/final-outputs/exit.json").is_file())

    def test_generation_mount_and_scene_replacement_refused(self):
        # Each scenario gets its own execution directory and immutable input.
        for fault in ("same_generation", "same_socket", "baseline_socket", "scene_restart", "same_agent", "wrong_service"):
            with self.subTest(fault=fault):
                directory = self.root/fault; directory.mkdir(mode=0o700)
                self.runner.out = directory
                self.hooks.directory = directory/"history"
                self.hooks._used = False; self.hooks.attempted = []; self.hooks.events = []
                self.hooks._commissioned = self.hooks._configured = True
                self.runner.calls = []; self.runner.ordinal = 0; self.runner.agent_pid = 500
                self.runner.current_manifest = "a"*64; self.runner.configs = {}; self.runner.remote = {}
                self.runner.scene = history_fixture()[3]
                self.runner.alter = fault
                with self.assertRaises(Refused): self.execute()
                self.assertLessEqual(self.operations().count("release-gate"), 1)
                self.assertNotIn("global_cleanup", self.operations())
                self.assertFalse(json.loads((self.hooks.directory/"result.json").read_bytes())["passed"])

    def test_failure_outputs_and_log_precede_parent_cleanup(self):
        self.prepare_hooks(); self.runner.fail = "cli"
        with self.assertRaises(Refused): self.execute()
        phase = self.hooks.directory/"before_restart"
        for name, _ in runtime.OUTPUTS: self.assertTrue((phase/"final-outputs"/name).is_file())
        self.assertTrue((self.hooks.directory/"phase-log.json").is_file())
        self.assertFalse(json.loads((self.hooks.directory/"result.json").read_bytes())["passed"])
        self.runner.fail = None; self.runner.finish()
        self.assertEqual(self.operations()[-1], "global_cleanup")
        self.assertNotIn("release-gate", self.operations())

    def test_deadline_keeps_all_available_failed_bytes_and_prevents_restart(self):
        self.prepare_hooks(); self.runner.alter = "wait_timeout"
        clock = [100.0]; self.runner.clock = clock
        with patch.object(runtime.time, "monotonic", side_effect=lambda: clock[0]), \
                patch.object(runtime.time, "sleep"), self.assertRaises(Refused):
            self.execute()
        phase = self.hooks.directory/"before_restart"
        for group in ("receipts", "final-outputs"):
            for name, _ in runtime.OUTPUTS: self.assertTrue((phase/group/name).is_file())
        self.assertNotIn("stop_agent", self.operations())

    def test_reader_failure_is_not_treated_as_absence_or_retried(self):
        self.prepare_hooks(); self.runner.alter = "reader_failed"
        with self.assertRaises(Refused): self.execute()
        self.assertEqual(self.operations().count("cli"), 1)
        self.assertNotIn("stop_agent", self.operations())
        saved = json.loads((self.hooks.directory/"before_restart/receipts/stdout.json").read_bytes())
        self.assertEqual(saved["status"], "failed")
        self.assertTrue((self.hooks.directory/"before_restart/final-outputs/stderr.log").is_file())

    def test_bundle_change_or_missing_opt_in_refused(self):
        with self.assertRaises(Refused): self.execute()
        self.assertEqual(self.runner.calls, [])
        self.prepare_hooks()
        (self.bundle/bundle_stage.CONTEXT/"app").write_bytes(b"changed")
        with self.assertRaises(Refused): self.execute()
        self.assertEqual(self.runner.calls, [])
        self.assertFalse(self.hooks.directory.exists())

    def test_foreign_runtime_never_receives_history_bytes(self):
        self.prepare_hooks(); self.runner.alter = "foreign"
        with self.assertRaises(Refused): self.execute()
        self.assertEqual(self.operations(), ["ownership"])

    def test_reader_binary_stream_is_chunked_exactly_with_one_final_hash(self):
        self.prepare_hooks()
        data = b"a"*(runtime.MAX_STDIN+17)
        (self.bundle/bundle_stage.CONTEXT/"app").write_bytes(data)
        expected = dict(size=len(data), sha256=r.sha(data))
        self.plan["binaries"][h.READER] = expected
        self.hooks.plan["binaries"][h.READER] = expected
        self.execute()
        uploads = [c for c in self.runner.calls if c["operation"] == "upload" and c["args"][0].endswith("/app")]
        self.assertEqual([c["bytes"] for c in uploads], [runtime.MAX_STDIN, 17])
        self.assertEqual([int(c["args"][1]) for c in uploads], [0, runtime.MAX_STDIN])
        self.assertTrue(all(c["args"][3] == r.sha(data) for c in uploads))
        self.assertEqual(self.runner.remote[runtime.REMOTE+"/"+bundle_stage.CONTEXT+"/app"], data)

    def test_launcher_bytes_rechecked_after_bundle_validation(self):
        self.prepare_hooks()
        (self.bundle/bundle_stage.CONTEXT/"Dockerfile").write_bytes(b"FROM unauthorized\n")
        with self.assertRaises(Refused): self.execute()
        self.assertEqual(self.runner.calls, [])
        self.assertFalse(self.hooks.directory.exists())

    def test_restart_cannot_renew_original_retention_even_with_same_bytes(self):
        self.prepare_hooks(); self.runner.alter = "retention_changed"
        with self.assertRaises(Refused): self.execute()
        result = json.loads((self.hooks.directory/"result.json").read_bytes())
        self.assertFalse(result["passed"])
        self.assertEqual(len(result["phases"]), 1)
        self.assertTrue((self.hooks.directory/"after_restart/final-outputs/stdout.json").is_file())

    def test_interrupted_cli_preserves_outputs_and_propagates_before_cleanup(self):
        self.prepare_hooks()
        with patch.object(self.runner, "cli", side_effect=KeyboardInterrupt), self.assertRaises(KeyboardInterrupt):
            self.execute()
        self.assertEqual(json.loads((self.hooks.directory/"result.json").read_bytes())["error_type"], "KeyboardInterrupt")
        for name, _ in runtime.OUTPUTS:
            self.assertTrue((self.hooks.directory/"before_restart/final-outputs"/name).is_file())
        self.assertNotIn("release-gate", self.operations())
        self.assertNotIn("global_cleanup", self.operations())


if __name__ == "__main__":
    unittest.main()
