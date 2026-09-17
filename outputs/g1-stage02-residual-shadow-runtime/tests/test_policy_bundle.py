from __future__ import annotations

import json
from pathlib import Path
import sys
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))

from runtime.policy_bundle import POLICY_SCHEMA, RUNTIME_ABI, verify_runtime_bundle


class PolicyBundleTests(unittest.TestCase):
    def test_current_bundle_implements_runtime_abi(self) -> None:
        identity = verify_runtime_bundle(ROOT / "bundle")
        self.assertEqual(identity.candidate_id, "reference-residual-gru-stage02-wide-u0016")
        self.assertEqual(identity.checkpoint_update, 16)
        self.assertEqual(identity.reference_frames, 1200)
        self.assertEqual(identity.public()["runtime_abi"], RUNTIME_ABI)

    def test_candidate_and_update_are_manifest_data_not_source_latches(self) -> None:
        source = (ROOT / "runtime" / "exact_policy.py").read_text()
        self.assertNotIn("reference-residual-gru-stage02-wide-u0016", source)
        self.assertNotIn("payload.get(\"update\") != 16", source)
        manifest = json.loads((ROOT / "bundle" / "POLICY.json").read_text())
        self.assertEqual(manifest["schema"], POLICY_SCHEMA)
        self.assertEqual(manifest["runtime_abi"], RUNTIME_ABI)

    def test_tampered_declared_hash_fails_closed(self) -> None:
        manifest = json.loads((ROOT / "bundle" / "POLICY.json").read_text())
        manifest["files"]["checkpoint.pt"] = "0" * 64
        with tempfile.TemporaryDirectory() as temporary:
            bundle = Path(temporary)
            (bundle / "POLICY.json").write_text(json.dumps(manifest))
            with self.assertRaisesRegex(ValueError, "missing or not regular|mismatch"):
                verify_runtime_bundle(bundle)


if __name__ == "__main__":
    unittest.main()
