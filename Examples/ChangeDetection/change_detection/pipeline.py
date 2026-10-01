"""Frozen reference, compensated residuals, region tracking, temporal decisions."""

from dataclasses import dataclass
import math

import cv2
import numpy as np

from .classifier import Classifier, RuleClassifier


@dataclass
class Config:
    width: int = 640
    difference_threshold: int = 24
    min_area: int = 90
    min_age: float = 8.0
    max_gap: float = 5.0
    max_speed: float = 0.015  # Frame diagonals per second.
    liquid_threshold: float = 0.70
    confirm_frames: int = 3
    max_regions: int = 16


@dataclass
class Track:
    id: int
    first_seen: float
    last_seen: float
    box: tuple[int, int, int, int]
    area: int
    speed: float = 0.0
    growth: float = 0.0
    confirmations: int = 0
    alerted: bool = False


def overlap(a, b):
    ax, ay, aw, ah = a
    bx, by, bw, bh = b
    intersection = max(0, min(ax + aw, bx + bw) - max(ax, bx)) * max(
        0, min(ay + ah, by + bh) - max(ay, by))
    return intersection / max(1, aw * ah + bw * bh - intersection)


def center(box):
    x, y, w, h = box
    return np.array([x + w / 2, y + h / 2])


class Pipeline:
    def __init__(self, classifier: Classifier | None = None, config: Config | None = None):
        self.classifier = classifier or RuleClassifier()
        self.config = config or Config()
        self.reset()

    def reset(self):
        self.reference = None
        self.last_timestamp = None
        self.tracks = {}
        self.next_id = 1
        self.events = []

    def process(self, image: np.ndarray, timestamp: float) -> dict:
        if not math.isfinite(timestamp) or (
            self.last_timestamp is not None and timestamp <= self.last_timestamp
        ):
            raise ValueError("Frame timestamps must be finite and strictly increasing")
        if image is None or image.dtype != np.uint8 or image.ndim != 3 or image.shape[2] != 3:
            raise ValueError("Expected a uint8 BGR image")
        h, w = image.shape[:2]
        if min(h, w) < 16:
            raise ValueError("Frame must be at least 16 pixels on each side")
        scale = min(1.0, self.config.width / w)
        frame = cv2.resize(image, (round(w * scale), round(h * scale)))
        if self.reference is not None and frame.shape != self.reference.shape:
            raise ValueError("Frame dimensions changed; reset the reference first")
        self.last_timestamp = timestamp
        initial = self.reference is None
        if initial:
            self.reference = frame.copy()
        before = self.reference
        h, w = frame.shape[:2]
        raw_delta = frame.astype(np.float32) - before.astype(np.float32)
        # A median resists small foreground changes. Freeze the reference so a
        # stationary spill never fades into an adaptive background model.
        offset = np.median(raw_delta, axis=(0, 1))
        corrected = np.clip(frame.astype(np.float32) - offset, 0, 255).astype(np.uint8)
        residual = cv2.absdiff(before, corrected).max(axis=2)
        residual = cv2.GaussianBlur(residual, (5, 5), 0)
        mask = (residual > self.config.difference_threshold).astype(np.uint8) * 255
        kernel = np.ones((3, 3), np.uint8)
        mask = cv2.morphologyEx(mask, cv2.MORPH_OPEN, kernel)
        mask = cv2.morphologyEx(mask, cv2.MORPH_CLOSE, kernel)
        coverage = float(np.count_nonzero(mask) / mask.size)
        raw_coverage = float(np.mean(np.max(np.abs(raw_delta), axis=2) > self.config.difference_threshold))
        brightness = float(np.mean(offset) / 255)
        shift = 0.0
        gray_before = cv2.cvtColor(before, cv2.COLOR_BGR2GRAY).astype(np.float32)
        gray_now = cv2.cvtColor(corrected, cv2.COLOR_BGR2GRAY).astype(np.float32)
        if not initial and np.std(gray_before) > 2:
            displacement, response = cv2.phaseCorrelate(gray_before, gray_now)
            if response > 0.30:
                shift = float(math.hypot(*displacement))
        unstable = coverage > 0.55 or shift > 3.0
        status = "reference captured" if initial else "watching"
        if unstable:
            status = "scene changed; restore camera or reset reference"
            self.tracks.clear()
        elif raw_coverage > 0.65 and abs(brightness) > 0.04:
            status = "global lighting change"

        self.tracks = {key: track for key, track in self.tracks.items()
                       if timestamp - track.last_seen <= self.config.max_gap}
        count, components, stats, _ = cv2.connectedComponentsWithStats(mask)
        candidates = [i for i in range(1, count) if stats[i, cv2.CC_STAT_AREA] >= self.config.min_area]
        candidates.sort(key=lambda i: int(stats[i, cv2.CC_STAT_AREA]), reverse=True)
        candidates = [] if unstable else candidates[:self.config.max_regions]
        used = set()
        regions = []
        diagonal = math.hypot(w, h)
        texture_reference = cv2.Laplacian(gray_before, cv2.CV_32F)
        texture_current = cv2.Laplacian(gray_now, cv2.CV_32F)
        for component in candidates:
            x, y, bw, bh, area = map(int, stats[component])
            box = (x, y, bw, bh)
            matches = []
            for track in self.tracks.values():
                if track.id in used:
                    continue
                distance = float(np.linalg.norm(center(box) - center(track.box)))
                iou = overlap(box, track.box)
                if iou > 0.05 or distance < 0.06 * diagonal:
                    matches.append((iou - distance / diagonal, track))
            if matches:
                track = max(matches, key=lambda item: item[0])[1]
                dt = timestamp - track.last_seen
                speed = float(np.linalg.norm(center(box) - center(track.box))) / diagonal / dt
                growth = (area - track.area) / max(track.area, 1) / dt
                track.speed = 0.5 * track.speed + 0.5 * speed
                track.growth = 0.5 * track.growth + 0.5 * growth
                track.box, track.area, track.last_seen = box, area, timestamp
            else:
                track = Track(self.next_id, timestamp, timestamp, box, area)
                self.next_id += 1
                self.tracks[track.id] = track
            used.add(track.id)
            padding = max(12, round(max(bw, bh) * 0.20))
            x0, y0 = max(0, x - padding), max(0, y - padding)
            x1, y1 = min(w, x + bw + padding), min(h, y + bh + padding)
            region_mask = (components[y0:y1, x0:x1] == component).astype(np.uint8) * 255
            crop_before = before[y0:y1, x0:x1].copy()
            crop_after = frame[y0:y1, x0:x1].copy()
            selected = components == component
            texture_before = float(texture_reference[selected].std())
            texture_after = float(texture_current[selected].std())
            features = {
                "age_seconds": timestamp - track.first_seen,
                "growth_rate": track.growth,
                "movement_speed": track.speed,
                "brightness_change_global": brightness,
                "area_fraction": area / mask.size,
                "mean_delta": float((gray_now - gray_before)[selected].mean() / 255),
                "mask_fill": area / (bw * bh),
                "texture_change": (texture_after - texture_before) / max(1, texture_before),
            }
            scores = self.classifier.predict(crop_before, crop_after, region_mask, features)
            label = max(scores, key=scores.get)
            if scores[label] < getattr(self.classifier, "unknown_threshold", 0.0):
                label = "unknown"
            eligible = (label == "liquid" and scores["liquid"] >= self.config.liquid_threshold
                        and features["age_seconds"] >= self.config.min_age
                        and track.speed <= self.config.max_speed)
            track.confirmations = track.confirmations + 1 if eligible else 0
            # A spill need not keep growing. Keep an observed candidate active
            # until the region disappears, the camera moves, or the user resets.
            alert = track.alerted or track.confirmations >= self.config.confirm_frames
            if alert and not track.alerted:
                self.events.insert(0, {"time": timestamp, "track_id": track.id,
                                       "message": "Persistent liquid candidate", "score": scores["liquid"]})
                self.events = self.events[:30]
            track.alerted = alert
            regions.append({"id": track.id, "box": box, "crop_box": (x0, y0, x1 - x0, y1 - y0),
                            "area": area, "label": label, "scores": scores,
                            "features": features, "alert": alert,
                            "before": crop_before, "after": crop_after, "mask": region_mask})
        # Missing observations break confirmation streaks, even before expiry.
        for track in self.tracks.values():
            if track.id not in used:
                track.confirmations = 0
                track.alerted = False
        if any(region["alert"] for region in regions):
            status = "persistent liquid candidate"
        elif regions and status == "watching":
            status = "tracking changes"
        return {"timestamp": timestamp, "status": status, "classifier": self.classifier.name,
                "score_kind": self.classifier.score_kind, "coverage": coverage,
                "brightness_change_global": brightness, "camera_shift_pixels": shift,
                "frame_size": [w, h], "regions": regions, "events": list(self.events), "before": before.copy(),
                "current": frame, "mask": mask, "residual": residual}
