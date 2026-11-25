"""Ownership and credential-routing regressions for the native test fixture."""
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from native_mission_site import ROOT, NativeMissionSite, fixture_action, selected_package


class NativeMissionFixtureTests(unittest.TestCase):
    def test_default_package_is_existing_live_office(self):
        path, relative = selected_package()
        self.assertEqual(path, ROOT / "examples/live-office/world.json")
        self.assertEqual(relative, "examples/live-office/world.json")

    def test_package_selection_refuses_escape_missing_directory_and_non_json(self):
        with tempfile.TemporaryDirectory() as folder:
            outside = Path(folder) / "world.json"
            outside.write_text("{}")
            for package in (outside, ROOT / "examples", "examples/not-present/world.json",
                            "examples/live-office/office.xml", "examples/../go.mod"):
                with self.subTest(package=package), self.assertRaises(ValueError):
                    selected_package(package)

    def test_resolved_symlink_cannot_escape_examples(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            (root / "examples").mkdir()
            outside = root / "world.json"
            outside.write_text("{}")
            (root / "examples/link.json").symlink_to(outside)
            with patch("native_mission_site.ROOT", root), self.assertRaises(ValueError):
                selected_package("examples/link.json")

    def test_invalid_package_fails_before_docker_inspection(self):
        with patch("native_mission_site.command") as docker, self.assertRaises(ValueError):
            NativeMissionSite(Path("unused"), "node", "scene", "/not-an-example.json")
        docker.assert_not_called()

    def test_selected_package_routes_host_commissioning_and_scene_consistently(self):
        class SceneReached(Exception):
            pass
        with tempfile.TemporaryDirectory() as folder:
            calls = []
            def command(args):
                calls.append(args)
                if args[:3] == ["docker", "image", "inspect"]:
                    return b"sha256:pinned"
                if args[:2] == ["docker", "port"]:
                    return b"127.0.0.1:12345"
                return b""
            with patch("native_mission_site.command", side_effect=command):
                site = NativeMissionSite(Path(folder) / "site", "node", "scene",
                                         "examples/sensor-office/world.json")
                def start(role, binary, args, *rest):
                    if role == "scene":
                        self.assertEqual(args[0], "examples/sensor-office/world.json")
                        raise SceneReached()
                    return role
                with patch.object(site, "start", side_effect=start), patch.object(site, "until"), self.assertRaises(SceneReached):
                    site.setup()
            lab = next(args for args in calls if args[:2] == ["go", "run"])
            self.assertEqual(lab[lab.index("--native-mission-package") + 1],
                             str(ROOT / "examples/sensor-office/world.json"))
            self.assertEqual(site.scene_package, "examples/sensor-office/world.json")

    def test_scene_validation_precedes_network_mutation_and_token_use(self):
        for changed in ("label", "port", "image", "running"):
            with self.subTest(changed=changed), tempfile.TemporaryDirectory() as folder:
                ready = {"mission_schema":"wendy.native-mission.site.v1", "network":"owned-peers",
                    "node_names":["owned-node"], "scene_name":"owned-scene", "ownership_label":"scope",
                    "scene_port":12345, "scene_image":"sha256:expected", "run_id":"fixture",
                    "package_sha256":"a"*64}
                path = Path(folder)/"ready.json"
                path.write_text(json.dumps(ready))
                # No token file exists. A malformed route must fail before any
                # token read, network disconnection or outbound HTTP request.
                scene = {"Image":"sha256:expected", "State":{"Running":True},
                    "NetworkSettings":{"Ports":{"8080/tcp":[{"HostIp":"127.0.0.1", "HostPort":"12345"}]}}}
                if changed == "port": scene["NetworkSettings"]["Ports"]["8080/tcp"][0]["HostPort"] = "54321"
                if changed == "image": scene["Image"] = "sha256:other"
                if changed == "running": scene["State"]["Running"] = False
                def docker(args):
                    self.assertIn("inspect", args)
                    if "--format" in args:
                        return b"other" if changed == "label" and args[-1] == "owned-scene" else b"scope"
                    self.assertEqual(args, ["docker", "inspect", "owned-scene"])
                    return json.dumps([scene]).encode()
                with patch("native_mission_site.command", side_effect=docker), patch("native_mission_site.urllib.request.urlopen") as http:
                    with self.assertRaises(ValueError): fixture_action(path, "partition")
                    http.assert_not_called()


if __name__ == "__main__": unittest.main()
