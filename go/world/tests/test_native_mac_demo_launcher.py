"""Launcher refusal and cleanup regressions without starting external processes."""
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock, patch

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location("native_mac_demo", ROOT / "scripts/native-mac-demo.py")
demo = importlib.util.module_from_spec(spec)
spec.loader.exec_module(demo)


class LauncherTests(unittest.TestCase):
    def test_changed_sources_refuse_before_process_launch(self):
        with patch.object(demo, "runtime_inputs", return_value={"new": "hash"}), patch.object(demo.subprocess, "Popen") as launch:
            with self.assertRaisesRegex(ValueError, "source changed"):
                demo.start(ROOT / ".local/unused", {"runtime_inputs": {}}, 60)
            launch.assert_not_called()

    def test_private_output_and_no_overwrite(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp).resolve()
            with patch.object(demo, "ROOT", root):
                with self.assertRaises(ValueError):
                    demo.new_directory(root / "public")
                output = demo.new_directory(root / ".local/demo")
                with self.assertRaises(FileExistsError):
                    demo.new_directory(output)

    def test_viewer_shutdown_error_still_stops_fixture(self):
        with tempfile.TemporaryDirectory() as tmp:
            folder = Path(tmp).resolve()
            site = folder / "site"
            site.mkdir()
            profile = site / "macos-mission-profile.json"
            profile.write_text("{}")
            (site / "ready.json").write_text(json.dumps({"mission_schema": "wendy.native-mission.site.v1",
                "run_id": "test", "native_mission_profiles": {"macos": str(profile)}}))
            fixture = Mock()
            fixture.poll.return_value = None
            viewer = Mock()
            viewer.poll.side_effect = [1, None]
            viewer.terminate.side_effect = OSError("viewer teardown failed")
            artifacts = {"runtime_inputs": {}, "node_image": "node", "scene_image": "scene", "app": "app",
                         "world_package": "examples/demo-office/world.json", "viewer_python": "mjpython", "viewer_libdir": "/lib"}
            with patch.object(demo, "ROOT", folder), patch.object(demo, "runtime_inputs", return_value={}), \
                    patch.object(demo, "captured", return_value=""), \
                    patch.object(demo.subprocess, "Popen", side_effect=[fixture, Mock(), viewer]) as launch:
                with self.assertRaisesRegex(OSError, "viewer teardown"):
                    demo.start(folder, artifacts, 60)
            self.assertEqual(launch.call_args_list[2].args[0][-2:], ["-ApplePersistenceIgnoreState", "YES"])
            self.assertIn('"stage": "macOS bootstrap"', (folder / "viewer.log").read_text())
            fixture.terminate.assert_called_once()
            fixture.wait.assert_called_once_with(timeout=120)


if __name__ == "__main__":
    unittest.main()
