"""Classifier contract and an explicitly untrained demonstration baseline."""

from typing import Protocol

import numpy as np

LABELS = (
    "liquid", "lighting", "shadow", "person", "moved_object", "new_object",
    "surface_damage", "smoke", "debris", "unknown",
)
FEATURE_NAMES = (
    "age_seconds", "growth_rate", "movement_speed", "brightness_change_global",
    "area_fraction", "mean_delta", "mask_fill", "texture_change",
)


def feature_vector(features: dict[str, float]) -> np.ndarray:
    """The same ordering and scaling are used for training and inference."""
    values = np.array([features[name] for name in FEATURE_NAMES], dtype=np.float32)
    if not np.isfinite(values).all():
        raise ValueError("Temporal features must be finite")
    values[0] /= 120.0
    values[2] *= 100.0
    return np.clip(values, -10.0, 10.0)


class Classifier(Protocol):
    name: str
    score_kind: str

    def predict(self, before: np.ndarray, after: np.ndarray, mask: np.ndarray,
                features: dict[str, float]) -> dict[str, float]: ...


class RuleClassifier:
    """Illustrates temporal cues. Scores are rules, not learned probabilities."""

    name = "Rule baseline · untrained"
    score_kind = "heuristic score"

    def predict(self, before, after, mask, features):
        scores = dict.fromkeys(LABELS, 0.0)
        scores["unknown"] = 0.55
        if features["movement_speed"] > 0.010:
            scores["shadow"] = 0.65 if features["mean_delta"] < 0 else 0.25
        elif features["mask_fill"] > 0.85:
            scores["new_object"] = 0.60
        elif features["mean_delta"] < -0.055:
            # Darkness and persistence alone cannot distinguish a stain/shadow.
            scores["liquid"] = 0.45
            if features["age_seconds"] >= 6 and features["growth_rate"] > 0.003:
                scores["liquid"] = 0.80
        return scores
