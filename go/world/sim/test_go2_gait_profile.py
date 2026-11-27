"""Explicit profile admission only; these checks do not prove gait dynamics."""
import copy
import hashlib
import json
import unittest

from sim.go2_bridge.protocol import Configuration, Refused, encoded, strict
from sim.test_go2_bridge import configuration
from sim.test_go2_tilt import tilted_configuration


class GaitProfileTests(unittest.TestCase):
    def test_explicit_bound_version_and_legacy_parity(self):
        cases = [
            ("legacy_absent", "isotropic-v1", None, True, False),
            ("legacy_zero", "isotropic-v1", "0", True, False),
            ("legacy_nonzero", "isotropic-v1", "0.1", False, False),
            ("legacy_null", "isotropic-v1", "null", False, False),
            ("legacy_v1_null", "isotropic-v1", "null", False, True),
            ("legacy_v1_zero", "isotropic-v1", "0", True, True),
            ("directional_override", "directional-v1", "0.1", False, False),
            ("gait_v1", "isotropic-gait-v1", "0.15", False, True),
            ("gait_absent", "isotropic-gait-v1", None, False, False),
            ("gait_zero", "isotropic-gait-v1", "0", False, False),
            ("gait_null", "isotropic-gait-v1", "null", False, False),
            ("gait_bool", "isotropic-gait-v1", "true", False, False),
            ("gait_negative", "isotropic-gait-v1", "-0.01", False, False),
            ("gait_over", "isotropic-gait-v1", "0.200001", False, False),
            ("gait_overflow", "isotropic-gait-v1", "1e1000", False, False),
            ("gait_nan", "isotropic-gait-v1", "NaN", False, False),
            ("gait_valid", "isotropic-gait-v1", "0.15", True, False),
            ("gait_upper_bound", "isotropic-gait-v1", "0.2", True, False),
        ]
        for name, model, number, admitted, legacy in cases:
            with self.subTest(case=name):
                cfg, alignment, raw = configuration() if legacy else tilted_configuration()
                profile = strict(raw)
                profile["motion_envelope"] = model
                raw = encoded(profile)
                if number is not None:
                    raw = raw[:-1] + b',"max_measured_lateral_mps":' + number.encode() + b'}'
                binding = copy.deepcopy(cfg.binding)
                binding["profile_sha256"] = hashlib.sha256(raw).hexdigest()
                if admitted:
                    current = Configuration(binding, alignment, raw)
                    self.assertEqual(current.profile["motion_envelope"], model)
                    self.assertEqual("max_measured_lateral_mps" in current.profile, number is not None)
                else:
                    with self.assertRaises(Refused):
                        Configuration(binding, alignment, raw)

    def test_legacy_bytes_and_explicit_digest_binding(self):
        cfg, alignment, raw = configuration()
        self.assertNotIn("max_measured_lateral_mps", cfg.profile)
        self.assertEqual(encoded(cfg.profile), raw)
        changed = dict(cfg.profile, motion_envelope="isotropic-gait-v1", max_measured_lateral_mps=.15)
        with self.assertRaisesRegex(Refused, "configuration_digest"):
            Configuration(cfg.binding, alignment, encoded(changed))
        for model in ("isotropic-v1", "directional-v1"):
            for value in (None, True, float("nan"), float("inf")):
                with self.subTest(model=model, value=repr(value)):
                    profile = dict(cfg.profile, motion_envelope=model, max_measured_lateral_mps=value)
                    # Deliberate invalid JSON numbers exercise strict decoder too.
                    bad = json.dumps(profile, allow_nan=True).encode()
                    binding = dict(cfg.binding, profile_sha256=hashlib.sha256(bad).hexdigest())
                    with self.assertRaises(Refused):
                        Configuration(binding, alignment, bad)

    def test_configuration_does_not_alias_commission_input(self):
        original, alignment, raw = tilted_configuration()
        profile = dict(original.profile, motion_envelope="isotropic-gait-v1", max_measured_lateral_mps=.15)
        raw = encoded(profile)
        binding = dict(original.binding, profile_sha256=hashlib.sha256(raw).hexdigest())
        cfg = Configuration(binding, alignment, raw)
        binding["profile_sha256"] = "0" * 64
        profile["max_measured_lateral_mps"] = .2
        self.assertEqual(cfg.profile["max_measured_lateral_mps"], .15)
        self.assertEqual(cfg.binding["profile_sha256"], hashlib.sha256(raw).hexdigest())


if __name__ == "__main__":
    unittest.main()
