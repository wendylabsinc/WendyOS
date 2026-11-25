"""Argument and isolation checks; no native runtime is needed for these tests."""
from types import SimpleNamespace
import unittest
from unittest.mock import Mock

from sim.mujoco_compat import LegacyMultiRayAPI, legacy_multiray


class LegacyMultiRayReview(unittest.TestCase):
    def test_only_nullable_normal_argument_is_inserted(self):
        call = Mock(return_value="native result")
        native = SimpleNamespace(__version__="4.0.0", mj_multiRay=call, MjData=object())
        source = SimpleNamespace(mujoco=native)
        arguments = [object() for _ in range(11)]
        with legacy_multiray(source, native) as metadata:
            self.assertIs(source.mujoco.MjData, native.MjData)
            self.assertIs(native.mj_multiRay, call)
            self.assertEqual(source.mujoco.mj_multiRay(*arguments), "native result")
            self.assertEqual(metadata["native_version"], "4.0.0")
            self.assertRegex(metadata["source_sha256"], r"^[0-9a-f]{64}$")
        self.assertIs(source.mujoco, native)
        call.assert_called_once()
        actual = call.call_args.args
        self.assertEqual(len(actual), 12)
        self.assertIsNone(actual[9])
        for original, forwarded in zip(arguments, actual[:9] + actual[10:]):
            self.assertIs(original, forwarded)

    def test_native_failure_restores_source_binding(self):
        native = SimpleNamespace(__version__="4.0.0", mj_multiRay=Mock(side_effect=RuntimeError("native failure")))
        source = SimpleNamespace(mujoco=native)
        with self.assertRaisesRegex(RuntimeError, "native failure"):
            with legacy_multiray(source, native):
                source.mujoco.mj_multiRay(*([None] * 11))
        self.assertIs(source.mujoco, native)

    def test_unknown_versions_and_already_changed_module_refused(self):
        for version in (None, "3.3.7", "4.0.1", "4.0.0-dev"):
            with self.subTest(version=version), self.assertRaises(ValueError):
                LegacyMultiRayAPI(SimpleNamespace(__version__=version))
        native = SimpleNamespace(__version__="4.0.0")
        other = object()
        source = SimpleNamespace(mujoco=other)
        with self.assertRaises(ValueError):
            with legacy_multiray(source, native):
                self.fail("changed source module was admitted")
        self.assertIs(source.mujoco, other)


if __name__ == "__main__":
    unittest.main()
