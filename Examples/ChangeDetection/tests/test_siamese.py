import importlib.util
import json
from pathlib import Path
import tempfile
import unittest

import numpy as np

HAS_TORCH = importlib.util.find_spec("torch") is not None and importlib.util.find_spec("torchvision") is not None


@unittest.skipUnless(HAS_TORCH, "Install requirements-training.txt to test the Siamese path")
class SiameseTests(unittest.TestCase):
    def test_training_step_freezes_encoder_and_checkpoint_reloads(self):
        import torch
        from change_detection.classifier import FEATURE_NAMES, LABELS
        from change_detection.siamese import IMAGE_SIZE, SiameseClassifier, SiameseNetwork, tensors
        torch.set_num_threads(1)
        model = SiameseNetwork(pretrained=False).train()
        before = np.full((40, 50, 3), 150, np.uint8)
        after = before.copy()
        after[10:30, 10:30] = 50
        mask = np.zeros((40, 50), np.uint8)
        mask[10:30, 10:30] = 255
        features = dict.fromkeys(FEATURE_NAMES, 0.0)
        inputs = [value.unsqueeze(0) for value in tensors(before, after, mask, features)]
        self.assertFalse(model.encoder.training)
        statistics = model.encoder.bn1.running_mean.clone()
        head_before = model.head[0].weight.detach().clone()
        optimizer = torch.optim.Adam(model.head.parameters(), lr=0.01)
        torch.nn.functional.cross_entropy(model(*inputs), torch.tensor([0])).backward()
        optimizer.step()
        self.assertFalse(torch.equal(head_before, model.head[0].weight))
        self.assertTrue(torch.equal(statistics, model.encoder.bn1.running_mean))
        self.assertTrue(all(p.grad is None for p in model.encoder.parameters()))
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "model.pt"
            checkpoint = {"version": 1, "labels": list(LABELS), "feature_names": list(FEATURE_NAMES),
                          "image_size": IMAGE_SIZE, "state_dict": model.state_dict()}
            torch.save(checkpoint, path)
            classifier = SiameseClassifier(path)
            scores = classifier.predict(before, after, mask, features)
            self.assertAlmostEqual(sum(scores.values()), 1, places=5)
            self.assertEqual(set(scores), set(LABELS))
            checkpoint["feature_names"] = list(reversed(FEATURE_NAMES))
            torch.save(checkpoint, path)
            with self.assertRaisesRegex(ValueError, "contract"):
                SiameseClassifier(path)

    def test_manifest_rejects_camera_session_leakage(self):
        from change_detection.classifier import FEATURE_NAMES
        from change_detection.train import read_manifest
        row = {"label": "liquid", "scene_id": "pump-room-camera-1",
               "features": dict.fromkeys(FEATURE_NAMES, 0.0)}
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "samples.jsonl"
            path.write_text("\n".join(json.dumps({**row, "split": split}) for split in ("train", "val")))
            with self.assertRaisesRegex(ValueError, "Scene leakage"):
                read_manifest(path)
            path.write_text("\n".join(json.dumps({**row, "split": split, "scene_id": split}) for split in ("train", "val")))
            self.assertEqual(len(read_manifest(path)), 2)


if __name__ == "__main__":
    unittest.main()
