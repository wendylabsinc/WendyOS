"""Audio change detection from PCM, independent of scenario names and input type.

This is an untrained spectral baseline. Candidate names describe demo signatures,
not validated recognition of metal or a torn belt.
"""

from dataclasses import dataclass
import math

import numpy as np


ZONES = ("hopper", "conveyor")
LABELS = ("normal", "metal_impact", "belt_rip", "unknown", "silence", "clipping")
SAMPLE_RATE = 16000
WINDOW_SECONDS = 0.25


@dataclass(frozen=True)
class AudioConfig:
    calibration_seconds: float = 3.0
    anomaly_threshold: float = 6.0
    metal_confirm_seconds: float = 0.5
    rip_confirm_seconds: float = 0.75
    clear_seconds: float = 1.0
    silence_dbfs: float = -65.0


def measure(samples, sample_rate):
    """Physical units are dBFS, Hz and dimensionless ratios. No dB SPL claim."""
    x = np.asarray(samples, dtype=np.float64)
    if x.ndim != 1 or len(x) != round(sample_rate * WINDOW_SECONDS):
        raise ValueError("Audio requires complete 250 ms mono windows")
    if not 8000 <= sample_rate <= 96000 or not np.isfinite(x).all() or np.max(np.abs(x)) > 1:
        raise ValueError("Expected finite PCM in [-1, 1] at 8–96 kHz")
    clip_fraction = float(np.mean(np.abs(x) >= 0.999))
    x = x - x.mean()
    rms = float(np.sqrt(np.mean(x * x)))
    # Short overlapping FFTs preserve the spectrum of impacts near window edges.
    size = round(sample_rate * 0.032)
    frames = np.lib.stride_tricks.sliding_window_view(x, size)[::size // 2]
    power = np.mean(np.abs(np.fft.rfft(frames * np.hanning(size), axis=1)) ** 2, axis=0)
    frequencies = np.fft.rfftfreq(size, 1 / sample_rate)
    high = power[frequencies >= 1500]
    total = float(power.sum()) + 1e-15
    high_total = float(high.sum())
    return {
        "rms_dbfs": float(20 * np.log10(max(rms, 1e-8))),
        "high_band_db": float(10 * np.log10(high_total / total + 1e-12)),
        "high_band_ratio": float(high_total / total),
        "high_band_flatness": float(np.exp(np.mean(np.log(high + 1e-15))) / (np.mean(high) + 1e-15)),
        "high_band_tonality": float(high.max() / max(high_total, 1e-15)),
        "centroid_hz": float(np.sum(frequencies * power) / total),
        "crest_factor": float(np.max(np.abs(x)) / max(rms, 1e-8)),
        "clip_fraction": clip_fraction,
    }, frequencies, power


class AudioPipeline:
    def __init__(self, zone, sample_rate=SAMPLE_RATE, config=None):
        if zone not in ZONES:
            raise ValueError("Audio zone must be hopper or conveyor")
        self.zone = zone
        self.sample_rate = sample_rate
        self.config = config or AudioConfig()
        self.reset()

    def reset(self):
        self.reference = []
        self.baseline = None
        self.last_timestamp = None
        self.break_continuity()

    def break_continuity(self):
        self.pending_label = None
        self.pending_seconds = 0.0
        self.first_evidence = None
        self.active = None
        self.quiet_seconds = 0.0

    def process(self, samples, timestamp):
        if not math.isfinite(timestamp) or timestamp < 0 or (
            self.last_timestamp is not None and timestamp <= self.last_timestamp
        ):
            raise ValueError("Audio timestamps must be finite, nonnegative and increasing")
        f, frequencies, power = measure(samples, self.sample_rate)
        if self.last_timestamp is not None and not math.isclose(
            timestamp - self.last_timestamp, WINDOW_SECONDS, abs_tol=0.03
        ):
            self.break_continuity()
            if self.baseline is None:
                self.reference.clear()
        self.last_timestamp = timestamp
        vector = np.array([f["rms_dbfs"], f["high_band_db"]])
        score = 0.0
        label = "normal"
        event = None
        quality = "clipping" if f["clip_fraction"] > 0.01 else (
            "silence" if f["rms_dbfs"] < self.config.silence_dbfs else None)
        if quality:
            label = quality
            self.break_continuity()
            if self.baseline is None:
                self.reference.clear()
        elif self.baseline is None:
            self.reference.append(vector)
            if len(self.reference) * WINDOW_SECONDS >= self.config.calibration_seconds:
                values = np.array(self.reference)
                median = np.median(values, axis=0)
                scale = np.maximum(1.4826 * np.median(np.abs(values - median), axis=0), [3.0, 3.0])
                self.baseline = (median, scale)
        else:
            median, scale = self.baseline
            score = float(np.max(np.abs(vector - median) / scale))
            label = "unknown" if score >= self.config.anomaly_threshold else "normal"
            # Energy must increase in the high band as well as deviate from normal.
            high_increase = float(vector.sum() - median.sum())
            if label == "unknown" and high_increase >= 12:
                if self.zone == "hopper" and f["high_band_ratio"] > 0.30 and f["high_band_tonality"] > 0.12:
                    label = "metal_impact"
                elif self.zone == "conveyor" and f["high_band_ratio"] > 0.45 and f["high_band_flatness"] > 0.35:
                    label = "belt_rip"
            candidate = label in ("metal_impact", "belt_rip")
            if candidate:
                self.quiet_seconds = 0.0
                if self.pending_label != label:
                    self.pending_seconds = 0.0
                    self.first_evidence = timestamp - WINDOW_SECONDS
                self.pending_label = label
                self.pending_seconds += WINDOW_SECONDS
                required = self.config.metal_confirm_seconds if label == "metal_impact" else self.config.rip_confirm_seconds
                if self.pending_seconds + 1e-9 >= required and self.active != label:
                    self.active = label
                    event = {"channel": f"audio_{self.zone}", "label": label,
                             "timestamp": timestamp, "first_evidence_timestamp": self.first_evidence,
                             "confirmation_seconds": timestamp - self.first_evidence,
                             "anomaly_score": score, "confidence": None,
                             "score_kind": "robust deviation, not probability"}
            else:
                self.pending_label = None
                self.pending_seconds = 0.0
                self.first_evidence = None
                self.quiet_seconds += WINDOW_SECONDS
                if self.quiet_seconds >= self.config.clear_seconds:
                    self.active = None
        ready = self.baseline is not None
        bins = np.array_split(np.arange(len(power)), 64)
        spectrum = [float(10 * np.log10(max(float(power[b].mean()), 1e-12))) for b in bins if len(b)]
        waveform = [float(np.max(np.abs(part))) for part in np.array_split(samples, 160)]
        return {"zone": self.zone, "timestamp": timestamp, "label": label,
                "status": quality or ("watching" if ready else "calibrating"),
                "calibrated": ready, "calibration_seconds": len(self.reference) * WINDOW_SECONDS,
                "calibration_required_seconds": self.config.calibration_seconds,
                "anomaly_score": score, "threshold": self.config.anomaly_threshold,
                "confirmation_seconds": self.pending_seconds, "active": self.active,
                "features": f, "waveform": waveform, "spectrum_db": spectrum,
                "spectrum_max_hz": self.sample_rate / 2, "event": event}
