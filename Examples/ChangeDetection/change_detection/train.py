"""Train on labeled paired crops. Run with python -m change_detection.train."""

import argparse
import json
from pathlib import Path

import cv2
import torch
from torch import nn
from torch.utils.data import DataLoader, Dataset

from .classifier import FEATURE_NAMES, LABELS, feature_vector
from .siamese import IMAGE_SIZE, SiameseNetwork, tensors


def read_manifest(path):
    rows = []
    scenes = {"train": set(), "val": set()}
    for line in Path(path).read_text().splitlines():
        if not line.strip():
            continue
        row = json.loads(line)
        if row.get("split") not in scenes or row.get("label") not in LABELS:
            raise ValueError("Each row needs split=train|val and a supported label")
        if not isinstance(row.get("scene_id"), str) or not row["scene_id"].strip():
            raise ValueError("Each row needs a scene_id identifying its camera/session")
        feature_vector(row["features"])
        scenes[row["split"]].add(row["scene_id"])
        rows.append(row)
    if not all(scenes.values()):
        raise ValueError("Supply both training and validation scenes")
    if scenes["train"] & scenes["val"]:
        raise ValueError("Scene leakage: a scene_id appears in both train and val")
    return rows


class CropDataset(Dataset):
    def __init__(self, rows, root):
        self.rows, self.root = rows, Path(root)

    def __len__(self):
        return len(self.rows)

    def __getitem__(self, index):
        row = self.rows[index]
        images = [cv2.imread(str(self.root / row[key]),
                             cv2.IMREAD_GRAYSCALE if key == "mask" else cv2.IMREAD_COLOR)
                  for key in ("before", "after", "mask")]
        if any(image is None for image in images):
            raise ValueError(f"Missing or invalid crop in sample {index}")
        if len({image.shape[:2] for image in images}) != 1:
            raise ValueError("Before, after, and mask must use identical crop coordinates")
        return (*tensors(*images, row["features"]), LABELS.index(row["label"]))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("manifest", type=Path)
    parser.add_argument("--output", type=Path, default=Path("models/change-classifier.pt"))
    parser.add_argument("--epochs", type=int, default=10)
    parser.add_argument("--batch-size", type=int, default=16)
    parser.add_argument("--seed", type=int, default=7)
    parser.add_argument("--device", default="cpu", help="cpu, cuda, or mps")
    args = parser.parse_args()
    if args.epochs < 1 or args.batch_size < 1:
        parser.error("epochs and batch-size must be positive")
    torch.manual_seed(args.seed)
    rows = read_manifest(args.manifest)
    loaders = {split: DataLoader(CropDataset([r for r in rows if r["split"] == split], args.manifest.parent),
                                batch_size=args.batch_size, shuffle=split == "train")
               for split in ("train", "val")}
    missing = set(LABELS) - {row["label"] for row in rows if row["split"] == "train"}
    if missing:
        print("No training examples for: " + ", ".join(sorted(missing)), flush=True)
    model = SiameseNetwork(pretrained=True).to(args.device)
    optimizer = torch.optim.AdamW([p for p in model.parameters() if p.requires_grad], lr=1e-3)
    criterion = nn.CrossEntropyLoss()
    best_loss = float("inf")
    args.output.parent.mkdir(parents=True, exist_ok=True)
    for epoch in range(args.epochs):
        metrics = {}
        for split, loader in loaders.items():
            model.train(split == "train")
            total_loss, correct, count = 0.0, 0, 0
            confusion = torch.zeros(len(LABELS), len(LABELS), dtype=torch.int64)
            with torch.set_grad_enabled(split == "train"):
                for *inputs, target in loader:
                    inputs = [value.to(args.device) for value in inputs]
                    target = target.to(args.device)
                    logits = model(*inputs)
                    loss = criterion(logits, target)
                    if split == "train":
                        optimizer.zero_grad()
                        loss.backward()
                        optimizer.step()
                    predictions = logits.argmax(1)
                    total_loss += loss.item() * len(target)
                    correct += int((predictions == target).sum())
                    count += len(target)
                    for truth, predicted in zip(target.cpu(), predictions.cpu()):
                        confusion[truth, predicted] += 1
            metrics[split] = {"loss": total_loss / count, "accuracy": correct / count,
                              "confusion": confusion.tolist()}
        print(json.dumps({"epoch": epoch + 1, **metrics}), flush=True)
        if metrics["val"]["loss"] < best_loss:
            best_loss = metrics["val"]["loss"]
            torch.save({"version": 1, "labels": list(LABELS), "feature_names": list(FEATURE_NAMES),
                        "image_size": IMAGE_SIZE, "state_dict": model.state_dict(),
                        "validation": metrics["val"], "seed": args.seed}, args.output)
    print(f"Saved best validation checkpoint to {args.output}", flush=True)


if __name__ == "__main__":
    main()
