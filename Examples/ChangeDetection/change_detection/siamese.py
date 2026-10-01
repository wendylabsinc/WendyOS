"""Optional shared ResNet encoder, mask branch, and temporal feature head."""

from pathlib import Path

import cv2
import torch
from torch import nn
from torchvision.models import ResNet18_Weights, resnet18

from .classifier import FEATURE_NAMES, LABELS, feature_vector

IMAGE_SIZE = 160


def tensors(before, after, mask, features):
    def image_tensor(image):
        rgb = cv2.cvtColor(cv2.resize(image, (IMAGE_SIZE, IMAGE_SIZE)), cv2.COLOR_BGR2RGB)
        value = torch.from_numpy(rgb.copy()).permute(2, 0, 1).float() / 255
        mean = torch.tensor([0.485, 0.456, 0.406])[:, None, None]
        std = torch.tensor([0.229, 0.224, 0.225])[:, None, None]
        return (value - mean) / std

    resized_mask = cv2.resize(mask, (IMAGE_SIZE, IMAGE_SIZE), interpolation=cv2.INTER_NEAREST)
    return (image_tensor(before), image_tensor(after),
            torch.from_numpy(resized_mask.copy()).float().unsqueeze(0) / 255,
            torch.from_numpy(feature_vector(features)))


class SiameseNetwork(nn.Module):
    def __init__(self, pretrained=False):
        super().__init__()
        backbone = resnet18(weights=ResNet18_Weights.DEFAULT if pretrained else None)
        backbone.fc = nn.Identity()
        self.encoder = backbone
        self.encoder.requires_grad_(False)
        self.mask_encoder = nn.Sequential(
            nn.Conv2d(1, 8, 5, stride=2, padding=2), nn.ReLU(),
            nn.Conv2d(8, 16, 3, stride=2, padding=1), nn.ReLU(),
            nn.AdaptiveAvgPool2d((2, 2)), nn.Flatten(),
        )
        self.head = nn.Sequential(
            nn.Linear(512 * 3 + 64 + len(FEATURE_NAMES), 128), nn.ReLU(),
            nn.Dropout(0.2), nn.Linear(128, len(LABELS)),
        )

    def train(self, mode=True):
        super().train(mode)
        # Frozen BatchNorm statistics matter as much as frozen parameters.
        self.encoder.eval()
        return self

    def forward(self, before, after, mask, features):
        with torch.no_grad():
            encoded_before, encoded_after = self.encoder(before), self.encoder(after)
        comparison = torch.cat((encoded_before, encoded_after,
                                torch.abs(encoded_after - encoded_before),
                                self.mask_encoder(mask), features), dim=1)
        return self.head(comparison)


def load_checkpoint(path: str | Path):
    checkpoint = torch.load(path, map_location="cpu", weights_only=True)
    if (checkpoint.get("version") != 1 or checkpoint.get("labels") != list(LABELS)
            or checkpoint.get("feature_names") != list(FEATURE_NAMES)
            or checkpoint.get("image_size") != IMAGE_SIZE):
        raise ValueError("Checkpoint does not match this sample's model contract")
    model = SiameseNetwork()
    model.load_state_dict(checkpoint["state_dict"])
    model.eval()
    return model


class SiameseClassifier:
    name = "Siamese ResNet-18 · trained checkpoint"
    score_kind = "model probability · uncalibrated"
    unknown_threshold = 0.5

    def __init__(self, path):
        self.model = load_checkpoint(path)

    def predict(self, before, after, mask, features):
        inputs = [value.unsqueeze(0) for value in tensors(before, after, mask, features)]
        with torch.inference_mode():
            probabilities = self.model(*inputs).softmax(dim=1)[0].tolist()
        return dict(zip(LABELS, probabilities))
