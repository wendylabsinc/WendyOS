#!/usr/bin/env python3
"""Offline checks for consumer entrypoints and HTTPS acquisition failure.

Generation, ancestry and source-copy policy live in service-protos. The existing
Shared gRPC API contracts job exercises that policy with real tools and schemas.
"""
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]


class SharedContractsTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.workspace = self.root / "workspace"
        self.bin = self.root / "bin"
        self.bin.mkdir()
        self.env = dict(os.environ, PATH=f"{self.bin}{os.pathsep}{os.environ['PATH']}",
                        TMPDIR=str(self.root))
        for name in ("scripts/contracts.sh", "go/scripts/generate-proto.sh",
                     "swift/Scripts/GenerateProto.sh", "Proto/shared-grpc-api-contracts.sh",
                     "contracts.json"):
            destination = self.workspace / name
            destination.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(ROOT / name, destination)

    def run_script(self, name, *args, **env):
        return subprocess.run(["bash", str(self.workspace / name), *args], cwd=self.root,
                              env=dict(self.env, **env), text=True, capture_output=True, timeout=20)

    def test_legacy_commands_delegate_full_check_or_generation_and_keep_errors(self):
        runner = self.workspace / "scripts/contracts.sh"
        runner.write_text('#!/bin/bash\nprintf "%s\\n" "$@"\nexit "${RESULT:-0}"\n')
        for script in ("go/scripts/generate-proto.sh", "swift/Scripts/GenerateProto.sh"):
            for args, expected in (([], "generate"), (["--shared-grpc-api-contracts-only"], "generate"),
                                   (["--check-shared-grpc-api-contracts"], "check")):
                with self.subTest(script=script, args=args):
                    result = self.run_script(script, *args)
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertEqual(result.stdout, expected + "\n")
                    failed = self.run_script(script, *args, RESULT="37")
                    self.assertEqual(failed.returncode, 37)
            for args in (["unknown"], ["--check-shared-grpc-api-contracts", "extra"]):
                failed = self.run_script(script, *args)
                self.assertNotEqual(failed.returncode, 0)
                self.assertIn("usage:", failed.stderr)
                self.assertEqual(failed.stdout, "")

    def test_old_sourced_pin_uses_config_and_checker_forwards_failure(self):
        runner = self.workspace / "scripts/contracts.sh"
        runner.write_text('#!/bin/bash\n[ "$*" = check ] || exit 99\nexit 37\n')
        script = self.workspace / "Proto/shared-grpc-api-contracts.sh"
        result = subprocess.run(["bash", "-c", 'source "$1"; printf "%s\\n" '
                                 '"$SHARED_GRPC_API_CONTRACTS_REF"; check_shared_grpc_api_contracts_ancestry',
                                 "fixture", str(script)], env=self.env, text=True, capture_output=True)
        self.assertEqual(result.returncode, 37)
        self.assertEqual(result.stdout.strip(), json.loads((self.workspace / "contracts.json").read_text())["commit"])

    def test_acquisition_failure_is_loud_and_preserves_consumer_files(self):
        git = self.bin / "git"
        git.write_text('''#!/bin/bash
if [[ "$*" == init* ]]; then exit 0; fi
[[ "$*" == *"https://github.com/wendylabsinc/service-protos.git"* ]] || exit 99
[[ "$*" == *"4cc0943e76be14d92f5f1a4a5fe5678cc77bb96e"* ]] || exit 98
echo 'injected HTTPS acquisition failure' >&2
exit 23
''')
        git.chmod(0o755)
        for name in ("go/proto/gen/sentinel.pb.go", "Proto/local.proto", "go/proto/gen/handwritten.go"):
            path = self.workspace / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text("preserve me\n")
        before = {str(p.relative_to(self.workspace)): p.read_bytes()
                  for p in self.workspace.rglob("*") if p.is_file()}
        for args in (["check"], ["generate"], ["bump", "0" * 40]):
            result = self.run_script("scripts/contracts.sh", *args)
            self.assertEqual(result.returncode, 23, result.stderr)
            self.assertIn("injected HTTPS acquisition failure", result.stderr)
            self.assertIn("bash scripts/contracts.sh generate", result.stderr)
            self.assertEqual(before, {str(p.relative_to(self.workspace)): p.read_bytes()
                                     for p in self.workspace.rglob("*") if p.is_file()})

    def test_invalid_runner_arguments_do_not_acquire_tools_or_sources(self):
        for args in ([], ["unknown"], ["bump"], ["check", "extra"], ["generate", "extra"]):
            result = self.run_script("scripts/contracts.sh", *args)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("usage:", result.stderr)
            self.assertFalse((self.workspace / "Build").exists())


if __name__ == "__main__":
    unittest.main()
