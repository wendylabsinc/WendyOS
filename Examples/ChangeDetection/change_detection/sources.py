"""Deterministic scenes and OpenCV camera/video inputs. Frames are BGR."""

from pathlib import Path
import time

import cv2
import numpy as np

SCENARIOS = ("liquid", "shadow", "lighting", "object", "empty", "camera_shift")


class DemoSource:
    name = "Synthetic scene · 4× time"
    interval = 0.5

    def __init__(self, scenario="liquid"):
        if scenario not in SCENARIOS:
            raise ValueError("Unknown demo scenario")
        self.scenario = scenario
        self.timestamp = -2.0
        rng = np.random.default_rng(7)
        floor = rng.normal(161, 3, (360, 640, 1))
        self.base = np.clip(floor + np.array([7, 3, -2]), 0, 255).astype(np.uint8)
        for x in range(0, 640, 80):
            cv2.line(self.base, (x, 0), (x, 360), (143, 141, 136), 2)
        for y in range(0, 360, 80):
            cv2.line(self.base, (0, y), (640, y), (143, 141, 136), 2)
        cv2.rectangle(self.base, (28, 25), (155, 97), (83, 90, 92), -1)
        cv2.rectangle(self.base, (38, 35), (145, 87), (107, 114, 116), 2)
        cv2.putText(self.base, "PUMP 04", (50, 67), cv2.FONT_HERSHEY_SIMPLEX, 0.45, (200, 210, 209), 1)
        cv2.line(self.base, (91, 96), (91, 148), (99, 104, 109), 14)
        cv2.line(self.base, (91, 148), (259, 148), (99, 104, 109), 14)
        cv2.circle(self.base, (259, 148), 10, (84, 88, 93), -1)

    def read(self):
        self.timestamp += 2.0
        t = self.timestamp
        frame = self.base.copy()
        if t < 4:
            return frame, t
        if self.scenario == "liquid":
            radius = 10 + min(t - 4, 100) * 1.0
            angles = np.linspace(0, 2 * np.pi, 70, endpoint=False)
            edge = 1 + 0.10 * np.sin(angles * 5) + 0.07 * np.cos(angles * 9)
            points = np.column_stack((275 + radius * edge * np.cos(angles),
                                      201 + radius * 0.63 * edge * np.sin(angles))).astype(np.int32)
            patch = np.zeros(frame.shape[:2], np.uint8)
            cv2.fillPoly(patch, [points], 255)
            frame[patch > 0] = (frame[patch > 0].astype(float) * 0.52).astype(np.uint8)
            cv2.ellipse(frame, (270, 195), (max(2, int(radius * .32)), 2), -12, 0, 180, (166, 176, 174), 1)
        elif self.scenario == "shadow":
            x = int(45 + ((t - 4) * 11) % 560)
            patch = np.zeros(frame.shape[:2], np.uint8)
            cv2.ellipse(patch, (x, 246), (39, 25), -20, 0, 360, 255, -1)
            frame[patch > 0] = (frame[patch > 0].astype(float) * 0.6).astype(np.uint8)
        elif self.scenario == "lighting":
            offset = int(48 * np.sin((t - 4) / 18))
            frame = np.clip(frame.astype(np.int16) + offset, 0, 255).astype(np.uint8)
        elif self.scenario == "object":
            cv2.rectangle(frame, (320, 205), (392, 260), (52, 102, 160), -1)
            cv2.line(frame, (356, 207), (356, 257), (94, 144, 197), 3)
        elif self.scenario == "camera_shift":
            frame = cv2.warpAffine(frame, np.float32([[1, 0, 12], [0, 1, 7]]),
                                   (640, 360), borderMode=cv2.BORDER_REFLECT)
        return frame, t

    def close(self):
        pass


class CaptureSource:
    def __init__(self, source: str, fps: float = 2.0):
        if not 0 < fps <= 30:
            raise ValueError("FPS must be in (0, 30]")
        target = int(source) if source.isdecimal() else source
        self.capture = cv2.VideoCapture(target)
        if not self.capture.isOpened():
            self.capture.release()
            raise ValueError("Could not open camera or video input")
        self.capture.set(cv2.CAP_PROP_BUFFERSIZE, 1)
        self.interval = 1 / fps
        self.is_file = isinstance(target, str) and Path(target).is_file() and not target.startswith("/dev/")
        self.name = "Video file" if self.is_file else "Camera / stream"
        self.native_fps = self.capture.get(cv2.CAP_PROP_FPS)
        if self.is_file and self.native_fps <= 0:
            self.capture.release()
            raise ValueError("Video must report a valid frame rate")
        self.stride = max(1, round(self.native_fps / fps)) if self.is_file else 1
        self.frame_index = -1
        self.start = time.monotonic()

    def read(self):
        # Decode skipped file frames so age/growth use video time, not CPU time.
        for _ in range(1 if self.frame_index < 0 else self.stride):
            ok, frame = self.capture.read()
            if not ok:
                raise EOFError("Video ended" if self.is_file else "Camera stopped returning frames")
            self.frame_index += 1
        timestamp = self.frame_index / self.native_fps if self.is_file else time.monotonic() - self.start
        return frame, timestamp

    def close(self):
        self.capture.release()
