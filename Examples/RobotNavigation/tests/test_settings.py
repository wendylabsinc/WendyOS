import copy
import json
from pathlib import Path
import unittest
from robot_navigation.settings import validate_settings


class SettingsTests(unittest.TestCase):
    def setUp(self):
        self.config = json.loads((Path(__file__).resolve().parents[1] / "config.json").read_text())

    def test_default_configuration_is_read_only(self):
        value = validate_settings(self.config)
        self.assertFalse(value["runtime"]["motion_enabled"])
        self.assertFalse(value["runtime"]["watchdog_commissioned"])
        self.assertEqual(value["motor"]["mode"], "disabled")

    def test_motor_cannot_feed_back_into_inputs(self):
        for topic in ("/robot_navigation/unsafe_cmd_vel", "/robot_navigation/safe_cmd_vel", "/odom"):
            with self.subTest(topic=topic), self.assertRaises(ValueError):
                self.config["motor"]["output_topic"] = topic
                validate_settings(self.config)

    def test_time_budget_covers_full_delivery_path(self):
        self.config["guard"]["reaction_seconds"] = .35
        with self.assertRaises(ValueError):
            validate_settings(self.config)

    def test_external_nav2_cannot_be_enabled_for_motion(self):
        self.config["runtime"]["motion_enabled"] = True
        self.config["motor"]["mode"] = "twist"
        self.config["start_nav2"] = False
        with self.assertRaises(ValueError):
            validate_settings(self.config)

    def test_required_sensors_cannot_be_removed(self):
        self.config["runtime"]["sensors"] = []
        with self.assertRaises(ValueError):
            validate_settings(self.config)
