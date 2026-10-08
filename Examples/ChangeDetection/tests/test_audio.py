import io
import json
from pathlib import Path
import tempfile
import time
from types import SimpleNamespace
import unittest
from unittest.mock import patch
import wave
import zipfile

import numpy as np

from change_detection.audio import AudioPipeline, measure, SAMPLE_RATE
from change_detection.audio_runtime import AudioRuntime, SimulatedConveyor, wav_bytes
from change_detection.audio_sources import DemoAudioSource, MicrophoneSource, WavAudioSource


class AudioPipelineTests(unittest.TestCase):
    def scene(self, scenario, windows=48):
        runtime = AudioRuntime(DemoAudioSource(scenario))
        for _ in range(windows):
            runtime.tick()
        return runtime

    def calibrated(self, zone="hopper"):
        pipeline = AudioPipeline(zone)
        source = DemoAudioSource("normal")
        for _ in range(12):
            samples, timestamp = source.read()
            pipeline.process(samples[:, 0], timestamp)
        return pipeline

    def anomaly(self, scenario, index=0):
        source = DemoAudioSource(scenario)
        for _ in range(21):
            samples, _ = source.read()
        return samples[:, index]

    def test_both_channels_detect_pcm_and_latch_stop_once(self):
        for scenario, zone, confirmed in (("metal_impact", "hopper", 5.5), ("belt_rip", "conveyor", 5.75)):
            with self.subTest(scenario=scenario):
                runtime = self.scene(scenario)
                self.assertEqual(len(runtime.events), 1)
                event = runtime.events[0]
                self.assertEqual(event["channel"], f"audio_{zone}")
                self.assertEqual(event["timestamp"], confirmed)
                self.assertIsNone(event["confidence"])
                self.assertTrue(event["synthetic"])
                self.assertGreaterEqual(event["stop_latency_ms"], 0)
                self.assertTrue(runtime.conveyor.stopped)
                self.assertIsNone(runtime.latest[zone]["active"])
                self.assertEqual(runtime.latest[zone]["label"], "normal")

    def test_negative_scenes_do_not_stop(self):
        for scenario in ("normal", "unknown", "silence", "clipping"):
            with self.subTest(scenario=scenario):
                runtime = self.scene(scenario, 28)
                self.assertFalse(runtime.events)
                self.assertFalse(runtime.conveyor.stopped)
                self.assertEqual(runtime.latest["hopper"]["label"], scenario)

    def test_short_impact_and_nonconsecutive_impacts_do_not_confirm(self):
        pipeline = self.calibrated()
        impact = self.anomaly("metal_impact")
        normal = self.anomaly("normal")
        self.assertIsNone(pipeline.process(impact, 3.25)["event"])
        self.assertIsNone(pipeline.process(normal, 3.5)["event"])
        self.assertIsNone(pipeline.process(impact, 3.75)["event"])
        self.assertIsNotNone(pipeline.process(impact, 4.0)["event"])

    def test_gap_and_bad_quality_break_confirmation(self):
        impact = self.anomaly("metal_impact")
        for middle, timestamp in ((None, 4.0), (np.zeros(4000), 3.75), (np.ones(4000), 3.75)):
            pipeline = self.calibrated()
            pipeline.process(impact, 3.25)
            if middle is not None:
                pipeline.process(middle, 3.5)
            self.assertIsNone(pipeline.process(impact, timestamp)["event"])
            self.assertIsNotNone(pipeline.process(impact, timestamp + .25)["event"])

    def test_calibration_is_frozen_and_invalid_signal_cannot_calibrate(self):
        pipeline = AudioPipeline("hopper")
        for i in range(20):
            result = pipeline.process(np.zeros(4000), (i + 1) * .25)
        self.assertFalse(result["calibrated"])
        self.assertEqual(result["features"]["high_band_ratio"], 0)
        pipeline = self.calibrated()
        reference = tuple(v.copy() for v in pipeline.baseline)
        impact = self.anomaly("metal_impact")
        for i in range(50):
            pipeline.process(impact, 3.25 + i * .25)
        for before, after in zip(reference, pipeline.baseline):
            np.testing.assert_array_equal(before, after)

    def test_zone_changes_candidate_interpretation_not_anomaly_score(self):
        impact = self.anomaly("metal_impact")
        hopper, conveyor = self.calibrated(), self.calibrated("conveyor")
        self.assertEqual(hopper.process(impact, 3.25)["label"], "metal_impact")
        self.assertEqual(conveyor.process(impact, 3.25)["label"], "unknown")

    def test_invalid_pcm_and_timestamps_rejected_without_mutation(self):
        pipeline = self.calibrated()
        normal = self.anomaly("normal")
        for timestamp in (3.0, 0, -1, float("nan"), float("inf")):
            with self.assertRaises(ValueError):
                pipeline.process(normal, timestamp)
        for samples in (normal[:20], normal.reshape(2, -1), np.full(4000, np.nan), np.full(4000, 1.2)):
            with self.assertRaises(ValueError):
                pipeline.process(samples, 3.25)
        self.assertEqual(pipeline.last_timestamp, 3.0)

    def test_sample_rates_measure_same_tone(self):
        for rate in (8000, 16000, 44100, 48000):
            t = np.arange(round(rate * .25)) / rate
            features, _, _ = measure(.2 * np.sin(2 * np.pi * 2400 * t), rate)
            self.assertAlmostEqual(features["rms_dbfs"], -16.99, places=1)
            self.assertAlmostEqual(features["centroid_hz"], 2400, delta=3)


class AudioRuntimeTests(unittest.TestCase):
    scene = AudioPipelineTests.scene

    def test_pause_breaks_streak_and_reset_clears_stop_and_recalibrates(self):
        runtime = self.scene("metal_impact", 21)
        self.assertFalse(runtime.events)
        runtime.control("pause", {"paused": True})
        runtime.control("pause", {"paused": False})
        runtime.tick()
        self.assertFalse(runtime.events)
        runtime.tick()
        self.assertTrue(runtime.conveyor.stopped)
        runtime.control("reset", {})
        self.assertFalse(runtime.conveyor.stopped)
        self.assertFalse(runtime.events)
        runtime.tick()
        self.assertFalse(runtime.latest["hopper"]["calibrated"])

    def test_export_is_retained_pcm_with_human_label_and_original_prediction(self):
        runtime = self.scene("metal_impact")
        event = runtime.events[0]
        data, _ = runtime.export({"id": event["id"], "label": "unknown"})
        with zipfile.ZipFile(io.BytesIO(data)) as archive:
            row = json.loads(archive.read(next(n for n in archive.namelist() if n.endswith("sample.json"))))
            self.assertEqual(row["label"], "unknown")
            self.assertEqual(row["predicted_label"], "metal_impact")
            with wave.open(io.BytesIO(archive.read(row["audio"]))) as clip:
                self.assertEqual(clip.getnframes(), 48000)
                self.assertEqual(clip.getframerate(), 16000)
                self.assertEqual(clip.getnchannels(), 1)
        runtime.control("reset", {})
        with self.assertRaises(ValueError):
            runtime.clip(event["id"])

    def test_latch_preserves_first_cause(self):
        conveyor = SimulatedConveyor()
        self.assertTrue(conveyor.trip({"id": 1})["stop_triggered"])
        self.assertFalse(conveyor.trip({"id": 2})["stop_triggered"])
        self.assertEqual(conveyor.trigger_event_id, 1)

    def test_evidence_does_not_concatenate_across_capture_gap(self):
        runtime = self.scene("metal_impact", 21)
        original_read = runtime.source.read
        def after_gap():
            samples, timestamp = original_read()
            return samples, timestamp + 2
        with patch.object(runtime.source, "read", side_effect=after_gap):
            runtime.tick()
            self.assertFalse(runtime.events)
            runtime.tick()
        event = runtime.events[0]
        self.assertEqual(event["clip_end_timestamp"] - event["clip_start_timestamp"], .5)
        with wave.open(io.BytesIO(runtime.clip(event["id"]))) as clip:
            self.assertEqual(clip.getnframes(), 8000)

    def test_worker_reports_input_failure_and_joins(self):
        runtime = AudioRuntime(DemoAudioSource())
        with patch.object(runtime.source, "read", side_effect=OSError("input disconnected")):
            with self.assertLogs(level="ERROR"):
                runtime.start()
                deadline = time.monotonic() + 2
                while not runtime.error and time.monotonic() < deadline:
                    time.sleep(.01)
                runtime.close()
        self.assertEqual(runtime.error, "input disconnected")
        self.assertFalse(runtime.thread.is_alive())


class AudioSourceTests(unittest.TestCase):
    def test_stereo_wav_roundtrip_and_mono_zone(self):
        source = DemoAudioSource("belt_rip")
        windows = [source.read()[0] for _ in range(36)]
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "source.wav"
            path.write_bytes(wav_bytes(np.concatenate(windows), SAMPLE_RATE))
            runtime = AudioRuntime(WavAudioSource(path))
            try:
                for _ in windows:
                    runtime.tick()
                self.assertEqual(runtime.events[0]["channel"], "audio_conveyor")
                self.assertFalse(runtime.events[0]["synthetic"])
                self.assertEqual(runtime.events[0]["timestamp"], 5.75)
                with self.assertRaises(EOFError):
                    runtime.tick()
                runtime.control("reset", {})
                runtime.tick()
                self.assertEqual(runtime.latest["conveyor"]["timestamp"], .25)
            finally:
                runtime.close()
            path.write_bytes(wav_bytes(np.concatenate(windows)[:, 1], SAMPLE_RATE))
            mono = WavAudioSource(path, "conveyor")
            self.assertEqual(mono.zones, ("conveyor",))
            self.assertEqual(mono.read()[0].shape, (4000, 1))
            mono.close()

    def test_partial_wav_and_unsupported_format(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "partial.wav"
            path.write_bytes(wav_bytes(np.zeros(100), SAMPLE_RATE))
            source = WavAudioSource(path)
            with self.assertRaisesRegex(EOFError, "incomplete"):
                source.read()
            source.close()
            with wave.open(str(path), "wb") as writer:
                writer.setnchannels(1)
                writer.setsampwidth(1)
                writer.setframerate(16000)
                writer.writeframes(b"\x80" * 4000)
            with self.assertRaisesRegex(ValueError, "16-bit"):
                WavAudioSource(path)

    def test_microphone_overflow_and_capture_errors_are_explicit(self):
        stream = SimpleNamespace(start=lambda: None, close=lambda: None)
        with patch.dict("sys.modules", {"sounddevice": SimpleNamespace(InputStream=lambda **kw: stream)}):
            source = MicrophoneSource()
        block = np.zeros((4000, 1), np.float32)
        timing = SimpleNamespace(inputBufferAdcTime=100.0)
        for i in range(9):
            timing.inputBufferAdcTime = 100 + i * .25
            source._capture(block, 4000, timing, None)
        with self.assertRaisesRegex(OSError, "overflow"):
            source.read()
        source.reset()
        source._capture(block, 4000, timing, "input overflow")
        with self.assertRaisesRegex(OSError, "capture failed"):
            source.read()
        source.close()
