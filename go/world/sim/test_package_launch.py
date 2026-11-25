"""Launcher boundary tests. Docker calls are mocked; no VM is contacted."""
import hashlib
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import time
import unittest
from unittest import mock

from sim import package_launch as launch
from sim import package_runner as runner


IMAGE = "sha256:" + "a" * 64


def fixture(folder):
    source = folder / "source"
    source.mkdir()
    model = b'<mujoco><worldbody><geom type="sphere" size="1"/></worldbody></mujoco>'
    asset = b"declared opaque data\x00\xff"
    package = {
        "schema_version": "wendy.world.package.v1", "site_id": "office", "package_revision": "test-v1",
        "frame_revision": "floor-v1", "provenance": "synthetic",
        "geographic_anchor": {"latitude_deg": 52, "longitude_deg": 5, "altitude_m": 0,
                              "altitude_datum": "WGS84-ellipsoid", "placement": "illustrative"},
        "local_frame": {"units": "metres", "handedness": "right", "up": "Y", "axes": ["east", "up", "south"]},
        "mujoco": {"path": "model.xml", "sha256": hashlib.sha256(model).hexdigest(), "to_local_position": ["x", "z", "-y"]},
        "assets": [{"path": "assets/data.bin", "sha256": hashlib.sha256(asset).hexdigest(), "size": len(asset)}],
    }
    raw = json.dumps(package, indent=1).encode() + b"\n"
    (source / "world.json").write_bytes(raw)
    (source / "model.xml").write_bytes(model)
    (source / "assets").mkdir()
    (source / "assets/data.bin").write_bytes(asset)
    (source / "unrelated-private.txt").write_bytes(b"not part of package")
    request = {"schema_version": runner.REQUEST_SCHEMA, "package_sha256": hashlib.sha256(raw).hexdigest(),
               "run_id": "launcher-unit", "steps": 2, "sample_every": 2, "body_names": [], "sensor_names": [],
               "initial_state": "model-default", "control": {"mode": "zero"}, "resource_policy": runner.POLICY}
    request_path = folder / "request.json"
    request_path.write_bytes(runner.canonical_bytes(request))
    return source / "world.json", request_path, request


def policy_record():
    return {"Image": IMAGE, "Config": {"User": "501:20"},
            "HostConfig": {"NetworkMode": "none", "ReadonlyRootfs": True, "Privileged": False,
                "Memory": launch.MEMORY_BYTES, "MemorySwap": launch.MEMORY_BYTES, "NanoCpus": 1_000_000_000,
                "PidsLimit": 64, "CapDrop": ["ALL"], "CapAdd": [], "Devices": [],
                "SecurityOpt": ["no-new-privileges"], "Tmpfs": {"/tmp": "rw,noexec,nosuid,size=16777216"},
                "PidMode": "", "IpcMode": "private", "UTSMode": ""},
            "Mounts": [{"Destination": destination, "Source": source, "Type": "bind", "RW": destination == "/output"}
                       for destination, source in {"/package": "/private/package", "/request.json": "/private/request.json", "/output": "/private/output"}.items()]}


class CommandTests(unittest.TestCase):
    def test_captures_only_stdout_and_bounds_combined_output(self):
        output = launch.command([sys.executable, "-c", "import sys;sys.stdout.write('ok');sys.stderr.write('diagnostic')"], timeout=2, limit=64)
        self.assertEqual(output, b"ok")
        for stream in ("stdout", "stderr"):
            with self.subTest(stream=stream), self.assertRaisesRegex(launch.LaunchError, "output exceeds"):
                launch.command([sys.executable, "-c", f"import sys;sys.{stream}.write('x'*65536);sys.{stream}.flush()"], timeout=2, limit=1000)
        with self.assertRaisesRegex(launch.LaunchError, "output exceeds"):
            launch.command([sys.executable, "-c", "import sys;sys.stdout.write('a'*600);sys.stderr.write('b'*600)"], timeout=2, limit=1000)

    def test_timeout_reaps_direct_child(self):
        real_popen = subprocess.Popen
        created = []
        def start(*args, **kwargs):
            process = real_popen(*args, **kwargs)
            created.append(process)
            return process
        started = time.monotonic()
        with mock.patch.object(launch.subprocess, "Popen", side_effect=start), self.assertRaisesRegex(launch.LaunchError, "timed out"):
            launch.command([sys.executable, "-c", "import time;time.sleep(30)"], timeout=.15)
        self.assertLess(time.monotonic() - started, 3)
        self.assertEqual(len(created), 1)
        self.assertIsNotNone(created[0].poll())
        self.assertTrue(created[0].stdout.closed)
        self.assertTrue(created[0].stderr.closed)

    def test_timeout_kills_pipe_holding_descendant_after_parent_exits(self):
        # An exited CLI parent does not mean that its inherited pipes are gone.
        with tempfile.TemporaryDirectory() as tmp:
            marker = Path(tmp) / "alive"
            child = "import time;from pathlib import Path; p=Path(%r); p.write_text('alive'); time.sleep(.7); p.write_text('survived'); time.sleep(10)" % str(marker)
            parent = "import subprocess,sys; p=subprocess.Popen([sys.executable,'-c',%r]); print(p.pid,flush=True)" % child
            real_popen = subprocess.Popen
            parent_process = []
            def start(*args, **kwargs):
                process = real_popen(*args, **kwargs)
                parent_process.append(process)
                return process
            try:
                with mock.patch.object(launch.subprocess, "Popen", side_effect=start), self.assertRaisesRegex(launch.LaunchError, "timed out"):
                    launch.command([sys.executable, "-c", parent], timeout=.3)
                self.assertTrue(marker.exists(), "descendant never entered the fixture")
                self.assertIsNotNone(parent_process[0].poll(), "fixture parent did not exit")
                time.sleep(.6)
                self.assertEqual(marker.read_text(), "alive", "descendant continued after timeout")
            finally:
                # Test-owned fallback also prevents a failing regression leaking.
                if parent_process:
                    try:
                        os.killpg(parent_process[0].pid, signal.SIGKILL)
                    except ProcessLookupError:
                        pass

    def test_nonzero_exit_is_refused(self):
        with self.assertRaisesRegex(launch.LaunchError, "failed \\(7\\)"):
            launch.command([sys.executable, "-c", "import sys;sys.stderr.write('data only');sys.exit(7)"], timeout=2)


class LocalImageTests(unittest.TestCase):
    def replies(self, context=None, info=None, image=None):
        return [json.dumps(context if context is not None else [{"Endpoints": {"docker": {"Host": "unix:///private/docker.sock"}}}]).encode(),
                json.dumps(info if info is not None else {"OSType": "linux", "MemoryLimit": True, "PidsLimit": True}).encode(),
                json.dumps(image if image is not None else [{"Id": IMAGE, "Os": "linux", "Config": {}}]).encode()]

    def test_local_existing_linux_image_only_and_no_pull(self):
        with mock.patch.dict(os.environ, {}, clear=True), mock.patch.object(launch, "command", side_effect=self.replies()) as command:
            self.assertEqual(launch.local_image("local-image:unit"), IMAGE)
        self.assertEqual([call.args[0][1:3] for call in command.call_args_list], [["context", "inspect"], ["info", "--format"], ["image", "inspect"]])

    def test_remote_and_unsupported_engines_images_refused(self):
        with mock.patch.dict(os.environ, {"DOCKER_HOST": "tcp://remote:2376"}, clear=True), mock.patch.object(launch, "command") as command:
            with self.assertRaises(launch.LaunchError):
                launch.local_image("local-image:unit")
            command.assert_not_called()
        variants = [
            self.replies(context=[{"Endpoints": {"docker": {"Host": "ssh://remote"}}}]),
            self.replies(info={"OSType": "windows", "MemoryLimit": True, "PidsLimit": True}),
            self.replies(info={"OSType": "linux", "MemoryLimit": False, "PidsLimit": True}),
            self.replies(info={"OSType": "linux", "MemoryLimit": True, "PidsLimit": False}),
            self.replies(image=[{"Id": IMAGE, "Os": "linux", "Config": {"Volumes": {"/data": {}}}}]),
            self.replies(image=[{"Id": "mutable-name", "Os": "linux", "Config": {}}]),
        ]
        for replies in variants:
            with self.subTest(replies=replies), mock.patch.dict(os.environ, {}, clear=True), mock.patch.object(launch, "command", side_effect=replies), self.assertRaises(launch.LaunchError):
                launch.local_image("local-image:unit")
        for name in ("", "--pull=always", "x"*257):
            with self.subTest(name=name), mock.patch.object(launch, "command") as command, self.assertRaises(launch.LaunchError):
                launch.local_image(name)
            command.assert_not_called()


class StageAndPublicationTests(unittest.TestCase):
    def test_only_verified_inventory_is_staged_without_execution(self):
        with tempfile.TemporaryDirectory() as tmp:
            folder = Path(tmp)
            package, _, request = fixture(folder)
            target = folder / "staged"
            with mock.patch.object(launch, "command") as command:
                staged = launch.stage_package(package, request["package_sha256"], target)
            command.assert_not_called()
            self.assertEqual(staged["_digest"], request["package_sha256"])
            self.assertEqual({p.relative_to(target).as_posix() for p in target.rglob("*") if p.is_file()}, {"world.json", "model.xml", "assets/data.bin"})
            for name in ("world.json", "model.xml", "assets/data.bin"):
                self.assertEqual((target/name).read_bytes(), (package.parent/name).read_bytes())
            self.assertEqual(target.stat().st_mode & 0o777, 0o700)

    def test_wrong_digest_and_changed_staging_bytes_are_refused(self):
        with tempfile.TemporaryDirectory() as tmp:
            folder = Path(tmp)
            package, _, request = fixture(folder)
            with self.assertRaises(launch.LaunchError):
                launch.stage_package(package, "0"*64, folder/"wrong")
            self.assertFalse((folder/"wrong").exists())
            real_read = launch.secure_read
            def changed(root, relative, limit, name):
                value = real_read(root, relative, limit, name)
                return value + b"tampered" if relative == "assets/data.bin" else value
            with mock.patch.object(launch, "secure_read", side_effect=changed), self.assertRaises(launch.LaunchError):
                launch.stage_package(package, request["package_sha256"], folder/"changed")

    def test_atomic_publish_preserves_existing_and_racing_outputs(self):
        with tempfile.TemporaryDirectory() as tmp:
            folder = Path(tmp)
            target = folder/"result.json"
            launch.publish_new(target, b"first")
            self.assertEqual(target.read_bytes(), b"first")
            self.assertEqual(target.stat().st_mode & 0o777, 0o600)
            with self.assertRaises(launch.LaunchError):
                launch.publish_new(target, b"replacement")
            self.assertEqual(target.read_bytes(), b"first")
            race = folder/"race.json"
            real_link = os.link
            def competing_link(source, destination, **kwargs):
                Path(destination).write_bytes(b"competitor")
                return real_link(source, destination, **kwargs)
            with mock.patch.object(launch.os, "link", side_effect=competing_link), self.assertRaises(FileExistsError):
                launch.publish_new(race, b"ours")
            self.assertEqual(race.read_bytes(), b"competitor")
            self.assertFalse(list(folder.glob(".world-execution-*")))
            dangling = folder/"dangling"
            dangling.symlink_to(folder/"missing")
            with self.assertRaises(launch.LaunchError):
                launch.publish_new(dangling, b"replacement")
            self.assertTrue(dangling.is_symlink())


class ContainerPolicyTests(unittest.TestCase):
    def inspect(self, record):
        with mock.patch.object(launch, "command", return_value=json.dumps([record]).encode()):
            return launch.inspect_policy("unit", IMAGE, "501:20", {
                "/package": "/private/package", "/request.json": "/private/request.json", "/output": "/private/output"})

    def test_exact_limits_and_mount_roles(self):
        self.inspect(policy_record())
        for key, value in {"NetworkMode": "host", "ReadonlyRootfs": False, "Privileged": True, "Memory": 0,
                           "MemorySwap": -1, "NanoCpus": 0, "PidsLimit": -1, "CapDrop": [],
                           "SecurityOpt": [], "PidMode": "host", "IpcMode": "host", "UTSMode": "host"}.items():
            record = policy_record(); record["HostConfig"][key] = value
            with self.subTest(key=key), self.assertRaises(launch.LaunchError):
                self.inspect(record)
        for change in ("extra", "writable-package", "volume", "wrong-image", "wrong-user"):
            record = policy_record()
            if change == "extra": record["Mounts"].append({"Destination": "/secret", "Source": "/private/secret", "Type": "bind", "RW": False})
            elif change == "writable-package": record["Mounts"][0]["RW"] = True
            elif change == "volume": record["Mounts"][0]["Type"] = "volume"
            elif change == "wrong-image": record["Image"] = "sha256:" + "b"*64
            else: record["Config"]["User"] = "0:0"
            with self.subTest(change=change), self.assertRaises(launch.LaunchError):
                self.inspect(record)

    def test_no_added_capabilities_devices_or_unbounded_tmpfs(self):
        for key, value in {"CapAdd": ["SYS_ADMIN"], "Devices": [{"PathOnHost": "/dev/test"}],
                           "Tmpfs": {"/tmp": "rw,exec,suid,size=1073741824"}}.items():
            record = policy_record(); record["HostConfig"][key] = value
            with self.subTest(key=key), self.assertRaises(launch.LaunchError):
                self.inspect(record)

    def test_duplicate_destination_is_not_hidden_by_map_conversion(self):
        record = policy_record()
        record["Mounts"].insert(0, {"Destination": "/package", "Source": "/private/wrong", "Type": "bind", "RW": True})
        with self.assertRaises(launch.LaunchError):
            self.inspect(record)

    def test_sources_match_exact_staged_paths(self):
        for index in range(3):
            record = policy_record()
            record["Mounts"][index]["Source"] = "/private/unrelated"
            with self.subTest(index=index), self.assertRaises(launch.LaunchError):
                self.inspect(record)


class LaunchOutcomeTests(unittest.TestCase):
    def scenario(self, folder, *, failing=None, state_change=None):
        package, request_path, request = fixture(folder)
        destination = folder / "execution.json"
        report = {"schema_version": runner.RESULT_SCHEMA, "request": request,
                  "request_sha256": runner.digest(request), "package": {"sha256": request["package_sha256"],
                      "schema_version": "wendy.world.package.v1", "site_id": "office", "package_revision": "test-v1", "frame_revision": "floor-v1"},
                  "validation": {"inventory_verified": True, "native_compiled": True, "simulation_completed": True}}
        events = []
        mounts = {}
        state = {"Running": False, "ExitCode": 0, "OOMKilled": False}
        if state_change:
            state.update(state_change)
        def command(args, **kwargs):
            action = args[1]
            events.append(action)
            if action == "create":
                self.assertIn(IMAGE, args, "create used a mutable image tag")
                self.assertNotIn("--privileged", args)
                for index, value in enumerate(args):
                    if value == "--mount":
                        details = dict(p.split("=", 1) for p in args[index+1].split(",") if "=" in p)
                        mounts[details["dst"]] = Path(details["src"])
                self.assertEqual(set(mounts), {"/package", "/request.json", "/output"})
                self.assertFalse((mounts["/package"]/"unrelated-private.txt").exists())
                self.assertEqual(json.loads(mounts["/request.json"].read_bytes()), request)
            if action == failing:
                raise launch.LaunchError("mock command failure")
            if action == "start":
                self.assertEqual(kwargs.get("timeout"), launch.CONTAINER_WALL_SECONDS)
                (mounts["/output"]/"result.json").write_bytes(runner.canonical_bytes(report))
            if action == "inspect":
                return json.dumps([{"State": state}]).encode()
            return b""
        def policy(*args, **kwargs):
            events.append("policy")
            if failing == "policy":
                raise launch.LaunchError("mock policy refusal")
            return {"image": IMAGE, "network_mode": "none"}
        real_publish = launch.publish_new
        def publish(path, data):
            events.append("publish")
            return real_publish(path, data)
        return package, request_path, request, destination, report, events, command, policy, publish, mounts

    def invoke(self, scenario, *, invalid_result=False):
        package, request_path, request, destination, report, events, command, policy, publish, _ = scenario
        def validate(value, expected):
            events.append("validate-result")
            self.assertEqual(expected, request)
            if invalid_result:
                raise runner.RunnerError("mock strict result refusal")
            return value
        with mock.patch.object(launch.os, "getuid", return_value=501), mock.patch.object(launch.os, "getgid", return_value=20), \
                mock.patch.object(launch, "local_image", return_value=IMAGE), \
                mock.patch.object(launch, "inspect_policy", side_effect=policy), \
                mock.patch.object(launch, "command", side_effect=command), \
                mock.patch.object(launch, "publish_new", side_effect=publish), \
                mock.patch.object(launch, "validate_result", side_effect=validate, create=True):
            return launch.launch(package, request_path, "unit:local", destination)

    def test_success_requires_strict_result_and_cleanup_before_publication(self):
        with tempfile.TemporaryDirectory() as tmp:
            scenario = self.scenario(Path(tmp))
            result = self.invoke(scenario)
            _, _, _, destination, _, events, _, _, _, mounts = scenario
            self.assertEqual(events.count("create"), 1)
            self.assertEqual(events.count("start"), 1)
            self.assertEqual(events.count("rm"), 1)
            self.assertEqual(events.count("validate-result"), 1)
            self.assertLess(events.index("rm"), events.index("publish"))
            self.assertEqual(json.loads(destination.read_bytes()), result)
            self.assertTrue(all(not p.exists() for p in mounts.values()), "temporary mounts survived launch")

    def test_uncertain_create_and_running_failures_cleanup_without_publish_or_retry(self):
        for failure in ("create", "policy", "start", "inspect", "rm"):
            with self.subTest(failure=failure), tempfile.TemporaryDirectory() as tmp:
                scenario = self.scenario(Path(tmp), failing=failure)
                with self.assertRaises(launch.LaunchError):
                    self.invoke(scenario)
                events = scenario[5]
                self.assertEqual(events.count("create"), 1)
                self.assertEqual(events.count("rm"), 1)
                self.assertLessEqual(events.count("start"), 1)
                self.assertNotIn("publish", events)
                self.assertFalse(scenario[3].exists())

    def test_failed_state_and_invalid_report_do_not_publish(self):
        for state in ({"Running": True}, {"ExitCode": 1}, {"OOMKilled": True}, {"ExitCode": False}, {"Running": None}):
            with self.subTest(state=state), tempfile.TemporaryDirectory() as tmp:
                scenario = self.scenario(Path(tmp), state_change=state)
                with self.assertRaises(launch.LaunchError):
                    self.invoke(scenario)
                self.assertEqual(scenario[5].count("rm"), 1)
                self.assertNotIn("publish", scenario[5])
                self.assertFalse(scenario[3].exists())
        with tempfile.TemporaryDirectory() as tmp:
            scenario = self.scenario(Path(tmp))
            with self.assertRaises(runner.RunnerError):
                self.invoke(scenario, invalid_result=True)
            self.assertEqual(scenario[5].count("rm"), 1)
            self.assertNotIn("publish", scenario[5])
        with tempfile.TemporaryDirectory() as tmp:
            scenario = self.scenario(Path(tmp))
            scenario[4]["package"]["frame_revision"] = "other-frame"
            with self.assertRaisesRegex(launch.LaunchError, "metadata differs"):
                self.invoke(scenario)
            self.assertEqual(scenario[5].count("rm"), 1)
            self.assertNotIn("publish", scenario[5])

    def test_existing_output_is_preserved_without_any_docker_call(self):
        with tempfile.TemporaryDirectory() as tmp:
            folder = Path(tmp)
            package, request_path, _ = fixture(folder)
            destination = folder/"already.json"
            destination.write_bytes(b"prior evidence")
            with mock.patch.object(launch, "command") as command, self.assertRaises(launch.LaunchError):
                launch.launch(package, request_path, "unit:local", destination)
            command.assert_not_called()
            self.assertEqual(destination.read_bytes(), b"prior evidence")


if __name__ == "__main__":
    unittest.main()
