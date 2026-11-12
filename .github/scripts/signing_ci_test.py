#!/usr/bin/env python3
"""Execute CI signing scripts offline with fake native tools and credentials."""

import base64
import json
from pathlib import Path
import re
import shlex
import shutil
import subprocess
import sys
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[2]
TOOLS = Path(__file__).with_name("signing_ci_tools.py")


class SigningCITests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="wendy-signing-regression-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.bin = self.root / "bin"
        self.bin.mkdir()
        self.user_keychains = [str(self.root / "user/Login.keychain-db"), str(self.root / "user/Team Keychain.keychain-db")]
        for path in self.user_keychains:
            Path(path).parent.mkdir(exist_ok=True)
            Path(path).write_text("existing user keychain must not change")
        self.state = self.root / "state.json"
        self.state.write_text(json.dumps({
            "search": self.user_keychains,
            "keychains": {p: {"certificate": "C" * 40, "display_name": "Developer ID Application: Duplicate Name (TEAM)", "profiles": {"wendy-notary-profile": "existing user profile"}} for p in self.user_keychains},
            "calls": [],
        }))
        self.original_users = self.read_state()["keychains"].copy()
        for name in ("security", "codesign", "xcrun", "curl", "shasum", "openssl", "xcodebuild", "ditto", "spctl", "xcbeautify"):
            self.write_tool(self.bin / name, name)

    def write_tool(self, path, name):
        path.write_text("#!/bin/sh\nexec " + " ".join(map(shlex.quote, (sys.executable, str(TOOLS), name))) + ' "$@"\n')
        path.chmod(0o755)

    def read_state(self):
        return json.loads(self.state.read_text())

    def job(self, name="one", fingerprint="A" * 40):
        job = self.root / name
        job.mkdir()
        runtime = job / "swift/WendyAgentMac/Resources/runtime"
        runtime.mkdir(parents=True)
        for filename in ("vmlinuz-arm64", "initramfs-arm64.img"):
            (runtime / filename).write_text("not a real runtime artifact")
        scripts = job / "swift/Scripts"
        scripts.mkdir()
        for filename in ("SetupSigningCI.sh", "Build.sh"):
            shutil.copyfile(ROOT / "swift/Scripts" / filename, scripts / filename)
        # Native release validation and app launch are outside this fixture.
        # Build.sh itself, including signing, packaging and notary submission,
        # executes unchanged; these platform-only siblings are recording tools.
        for filename in ("ValidateMacRelease.sh", "SmokeTestMacReleaseLaunch.sh"):
            self.write_tool(scripts / filename, filename)
        for filename in ("WendyAgentMac/Support/WendyAgentMac.entitlements", "WendyAgentMac/WendyAgentMacNetProxy/WendyAgentMacNetProxy.entitlements"):
            dest = job / "swift" / filename
            dest.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(ROOT / "swift" / filename, dest)
        temp = job / "temp"
        temp.mkdir()
        env = {"PATH": str(self.bin) + ":/usr/bin:/bin", "RUNNER_TEMP": str(temp), "TMPDIR": str(temp),
               "GITHUB_OUTPUT": str(job / "outputs"), "SIGNING_FIXTURE_STATE": str(self.state), "SIGNING_FIXTURE_JOB": name,
               "SIGNING_FIXTURE_FINGERPRINT": fingerprint, "VERSION": "2026.11.12", "OUTPUT_DIR": str(job / "out"), "DERIVED_DATA_PATH": str(job / "derived")}
        for key in ("DEVELOPER_ID_CERTIFICATE", "DEVELOPER_ID_KEY", "WENDY_AGENT_MAC_PROVISIONING_PROFILE", "WENDY_AGENT_MAC_NET_PROXY_PROVISIONING_PROFILE"):
            env[key] = base64.b64encode(("fake " + name + " " + key).encode()).decode()
        env.update(NOTARY_APPLE_ID=f"{name}@example.invalid", NOTARY_TEAM_ID="FAKETEAM", NOTARY_PASSWORD="fake-password-" + name)
        return job, env

    def run_script(self, job, env, script):
        step_env = dict(env)
        if script == "Build.sh":
            # GitHub gives each step its own output file. Cleanup must consume
            # the setup step's paths even when the build emits other outputs.
            step_env["GITHUB_OUTPUT"] = str(job / "build-outputs")
        result = subprocess.run(["bash", str(job / "swift/Scripts" / script)], env=step_env, text=True, capture_output=True, timeout=15)
        for secret in ("NOTARY_PASSWORD", "DEVELOPER_ID_CERTIFICATE", "DEVELOPER_ID_KEY"):
            self.assertNotIn(env[secret], result.stdout + result.stderr)
        return result

    def setup_signing(self, job, env):
        result = self.run_script(job, env, "SetupSigningCI.sh")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        outputs = dict(line.split("=", 1) for line in Path(env["GITHUB_OUTPUT"]).read_text().splitlines())
        self.assertEqual(outputs["signing_identity"], env["SIGNING_FIXTURE_FINGERPRINT"])
        self.assertEqual(outputs["keychain_path"], str(Path(env["RUNNER_TEMP"]) / "wendy-signing.keychain-db"))
        env.update({key.upper(): value for key, value in outputs.items()})
        return outputs

    def workflow_cleanup(self, env):
        workflow = (ROOT / ".github/workflows/build.yml").read_text()
        step = re.search(r"^      - name: Clean up signing assets\n(.*?)(?=^      - |^  \S|\Z)", workflow, re.M | re.S)
        self.assertIsNotNone(step)
        self.assertRegex(step[1], r"(?m)^        if: always\(\)$")
        # Execute the committed run body, not a second cleanup implementation.
        body = re.search(r"(?m)^        run: \|\n((?:          .*\n|\n)+)", step[1])
        self.assertIsNotNone(body)
        script = "\n".join(line[10:] for line in body[1].splitlines())
        cleanup_env = {key: value for key, value in env.items() if key not in (
            "KEYCHAIN_PATH", "APP_PROVISIONING_PROFILE_PATH", "NET_PROXY_PROVISIONING_PROFILE_PATH")}
        output_file = Path(env["GITHUB_OUTPUT"])
        outputs = dict(line.split("=", 1) for line in output_file.read_text().splitlines()) if output_file.exists() else {}
        for variable, output in re.findall(r"^          (\w+): \$\{\{ steps.signing.outputs.(\w+) \}\}$", step[1], re.M):
            self.assertEqual(variable.lower(), output)
            cleanup_env[variable] = outputs.get(output, "")
        result = subprocess.run(["bash", "-c", script], env=cleanup_env, text=True, capture_output=True, timeout=10)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def assert_clean(self, env, remaining=()):
        self.assertEqual(list(Path(env["RUNNER_TEMP"]).glob("*.keychain-db")), [])
        for pattern in ("*.cer", "*.key", "*.provisionprofile"):
            self.assertEqual(list(Path(env["RUNNER_TEMP"]).glob(pattern)), [], pattern)
        state = self.read_state()
        self.assertEqual(set(state["keychains"]), set(self.user_keychains) | set(remaining))
        self.assertEqual(set(state["search"]), set(self.user_keychains) | set(remaining))
        for path in self.user_keychains:
            self.assertEqual(state["keychains"][path], self.original_users[path])
            self.assertEqual(Path(path).read_text(), "existing user keychain must not change")

    def test_duplicate_names_two_jobs_and_repeat_run_use_exact_scoped_identity(self):
        first, first_env = self.job()
        second, second_env = self.job("two", "B" * 40)
        self.setup_signing(first, first_env)
        self.setup_signing(second, second_env)
        # Both jobs' keychains and identically named notary profiles coexist.
        for job, env in ((first, first_env), (second, second_env)):
            result = self.run_script(job, env, "Build.sh")
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        calls = self.read_state()["calls"]
        self.assertEqual(sum(tool == "codesign" and "--sign" in args for _, tool, args in calls), 6)
        self.assertEqual(sum(tool == "xcrun" and args[:2] == ["notarytool", "submit"] for _, tool, args in calls), 2)
        self.workflow_cleanup(first_env)
        self.assert_clean(first_env, [second_env["KEYCHAIN_PATH"]])
        self.workflow_cleanup(second_env)
        self.assert_clean(second_env)
        Path(first_env["GITHUB_OUTPUT"]).unlink()
        self.setup_signing(first, first_env)
        self.workflow_cleanup(first_env)
        self.assert_clean(first_env)

    def test_setup_failure_and_cancellation_after_each_state_change_cleanup(self):
        points = ("security:create-keychain", "security:set-keychain-settings", "security:unlock-keychain", "curl",
                  "security:import:intermediate", "security:import:cer", "security:import:key", "security:set-key-partition-list",
                  "security:list-keychains:set", "security:find-identity", "xcrun:notarytool:store-credentials")
        for cancel in (False, True):
            for index, point in enumerate(points):
                with self.subTest(cancel=cancel, point=point):
                    job, env = self.job(f"stop-{int(cancel)}-{index}")
                    env.update(SIGNING_FIXTURE_STOP_AT=point, SIGNING_FIXTURE_CANCEL=str(int(cancel)))
                    result = self.run_script(job, env, "SetupSigningCI.sh")
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn([env["SIGNING_FIXTURE_JOB"], point, str(int(cancel))], self.read_state().get("stops", []))
                    self.workflow_cleanup(env)  # Failed setup has no outputs.
                    self.assert_clean(env)

    def test_build_failure_and_cancellation_always_cleanup_signing_state(self):
        for cancel in (False, True):
            for index, point in enumerate(("codesign:sign", "xcrun:notarytool:submit")):
                with self.subTest(cancel=cancel, point=point):
                    job, env = self.job(f"build-stop-{int(cancel)}-{index}")
                    self.setup_signing(job, env)
                    env.update(SIGNING_FIXTURE_STOP_AT=point, SIGNING_FIXTURE_CANCEL=str(int(cancel)))
                    result = self.run_script(job, env, "Build.sh")
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn([env["SIGNING_FIXTURE_JOB"], point, str(int(cancel))], self.read_state().get("stops", []))
                    self.workflow_cleanup(env)
                    self.assert_clean(env)

    def test_bad_intermediate_checksum_cleans_decoded_credentials(self):
        job, env = self.job()
        env["SIGNING_FIXTURE_BAD_CA"] = "1"
        result = self.run_script(job, env, "SetupSigningCI.sh")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("checksum mismatch", result.stdout)
        self.workflow_cleanup(env)
        self.assert_clean(env)

    def test_invalid_encoded_key_cleans_partially_decoded_inputs(self):
        job, env = self.job()
        env["DEVELOPER_ID_KEY"] = "%%%invalid base64%%%"
        result = self.run_script(job, env, "SetupSigningCI.sh")
        self.assertNotEqual(result.returncode, 0)
        self.workflow_cleanup(env)
        self.assert_clean(env)

    def test_original_display_name_selection_is_rejected(self):
        job, env = self.job()
        script = job / "swift/Scripts/SetupSigningCI.sh"
        original = script.read_text()
        before = "awk '/Developer ID Application/ { print $2; exit }'"
        self.assertEqual(original.count(before), 1)
        script.write_text(original.replace(before, "awk -F '\"' '/Developer ID Application/ { print $2; exit }'"))
        result = self.run_script(job, env, "SetupSigningCI.sh")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("Could not find a Developer ID Application identity", result.stdout)
        self.workflow_cleanup(env)
        self.assert_clean(env)

    def test_original_unscoped_notary_storage_and_submission_are_rejected(self):
        for index, filename in enumerate(("SetupSigningCI.sh", "Build.sh")):
            with self.subTest(script=filename):
                job, env = self.job(f"unscoped-{index}")
                if filename == "Build.sh":
                    self.setup_signing(job, env)
                script = job / "swift/Scripts" / filename
                original = script.read_text()
                before = ' \\\n' + ('  ' if filename == "SetupSigningCI.sh" else '    ') + '--keychain "$KEYCHAIN_PATH"'
                self.assertEqual(original.count(before), 1)
                script.write_text(original.replace(before, ""))
                result = self.run_script(job, env, filename)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("xcrun omitted --keychain", result.stderr)
                self.workflow_cleanup(env)
                self.assert_clean(env)


if __name__ == "__main__":
    unittest.main()
