#!/usr/bin/env python3
"""Admission tests only; real CLI execution belongs to test-readme-install.py."""
import importlib.util
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("readme_install", Path(__file__).with_name("test-readme-install.py"))
install = importlib.util.module_from_spec(spec)
spec.loader.exec_module(install)


class ReadmeInstallerAdmission(unittest.TestCase):
    def test_actual_readme_and_changed_command_refusal(self):
        text = (install.ROOT / "README.md").read_text()
        self.assertEqual(install.snippet(text)[1], "wendy --version")
        for modified in (text.replace("| bash", "| sh"), text + "\n```sh\n" +
                         install.INSTALL_URL + "\n```", text.replace("wendy --version", "wendy tour")):
            with self.assertRaisesRegex(RuntimeError, "README_INSTALL_BLOCK"):
                install.snippet(modified)

    def map(self, directory):
        path = directory / "map.json"
        path.write_text(json.dumps({"work": str(directory), "curl": "/usr/bin/curl",
                                   "origin": "http://127.0.0.1:43210",
                                   "urls": {install.INSTALL_URL: "/cli.sh"}}))
        return patch.dict(os.environ, {"README_INSTALL_MAP": str(path)})

    def test_exact_url_uses_real_curl_without_proxy_or_redirect_escape(self):
        with tempfile.TemporaryDirectory() as directory, self.map(Path(directory).resolve()), \
                patch.object(os, "execv") as execute:
            install.adapter(["-fsSL", install.INSTALL_URL])
            path, argv = execute.call_args.args
            self.assertEqual(path, "/usr/bin/curl")
            self.assertEqual(argv[-1], "http://127.0.0.1:43210/cli.sh")
            self.assertIn("--noproxy", argv)
            self.assertEqual(argv[argv.index("--max-redirs") + 1], "0")
            self.assertEqual(argv[argv.index("--max-time") + 1], "15")

    def test_unknown_url_and_flags_never_execute(self):
        with tempfile.TemporaryDirectory() as directory, self.map(Path(directory).resolve()), \
                patch.object(os, "execv") as execute:
            for args in (["-fsSL", "https://example.test/cli.sh"],
                         ["-fsSL", install.INSTALL_URL + "?redirect=1"],
                         ["--config", "/tmp/unowned"]):
                with self.assertRaises(RuntimeError):
                    install.adapter(args)
            execute.assert_not_called()

    def test_destination_escape_never_executes(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory).resolve()
            (root / "outside").symlink_to(root.parent, target_is_directory=True)
            with self.map(root), patch.object(os, "execv") as execute:
                for path in (root / ".." / "escape", root / "outside" / "escape"):
                    with self.assertRaisesRegex(RuntimeError, "outside owned work"):
                        install.adapter(["-fsSL", "-o", str(path), install.INSTALL_URL])
                execute.assert_not_called()


if __name__ == "__main__":
    unittest.main()
