"""Independent audio worker, evidence retention and an in-process stop simulator."""

from collections import deque
from datetime import datetime, timezone
import io
import logging
import math
import threading
import time
import wave
import zipfile
import json

import numpy as np

from .audio import AudioPipeline, LABELS, WINDOW_SECONDS
from .audio_sources import AUDIO_SCENARIOS, DemoAudioSource


def wav_bytes(samples, sample_rate):
    output = io.BytesIO()
    with wave.open(output, "wb") as writer:
        writer.setnchannels(1 if samples.ndim == 1 else samples.shape[1])
        writer.setsampwidth(2)
        writer.setframerate(sample_rate)
        writer.writeframes((np.clip(samples, -1, 1) * 32767).astype("<i2").tobytes())
    return output.getvalue()


class SimulatedConveyor:
    """A latch only. No hardware, fieldbus, PLC acknowledgement or safety claim."""

    def __init__(self):
        self.stopped = False
        self.trigger_event_id = None

    def trip(self, event):
        started = time.perf_counter()
        newly_stopped = not self.stopped
        self.stopped = True
        if newly_stopped:
            self.trigger_event_id = event["id"]
        return {"stop_triggered": newly_stopped, "conveyor_stopped": True,
                "stop_latency_ms": (time.perf_counter() - started) * 1000,
                "stop_transport": "in_process_simulation"}

    def state(self):
        return {"simulated": True, "stopped": self.stopped,
                "trigger_event_id": self.trigger_event_id}


class AudioRuntime:
    def __init__(self, source, scene_id="session-1"):
        self.source = source
        self.scene_id = scene_id
        self.lock = threading.RLock()
        self.stop = threading.Event()
        self.thread = None
        self.paused = False
        self.next_id = 1
        self._reset()

    def _reset(self):
        self.pipelines = {zone: AudioPipeline(zone, self.source.sample_rate) for zone in self.source.zones}
        self.latest = {}
        self.history = deque(maxlen=120)
        self.samples = deque(maxlen=round(3 / WINDOW_SECONDS))
        self.last_sample_timestamp = None
        self.events = deque(maxlen=30)
        self.evidence = {}
        self.conveyor = SimulatedConveyor()
        self.error = None

    def tick(self):
        with self.lock:
            samples, timestamp = self.source.read()
            if self.paused:
                # Keep draining live capture while paused; never replay old microphone buffers.
                return
            if self.last_sample_timestamp is not None and not math.isclose(
                timestamp - self.last_sample_timestamp, WINDOW_SECONDS, abs_tol=0.03
            ):
                # A WAV cannot express a capture gap. Retain only contiguous evidence.
                self.samples.clear()
            self.last_sample_timestamp = timestamp
            self.samples.append(samples.copy())
            for index, zone in enumerate(self.source.zones):
                result = self.pipelines[zone].process(samples[:, index], timestamp)
                event = result.pop("event")
                if event:
                    event.update({"id": self.next_id, "observed_at": datetime.now(timezone.utc).isoformat(),
                                  "synthetic": self.source.synthetic, "scene_id": self.scene_id,
                                  "sample_rate": self.source.sample_rate, "zone": zone,
                                  "features": result["features"].copy()})
                    self.next_id += 1
                    event.update(self.conveyor.trip(event))
                    clip = np.concatenate(list(self.samples))[:, index]
                    event["clip_start_timestamp"] = timestamp - len(clip) / self.source.sample_rate
                    event["clip_end_timestamp"] = timestamp
                    self.evidence[event["id"]] = wav_bytes(clip, self.source.sample_rate)
                    self.events.appendleft(event)
                    retained = {e["id"] for e in self.events}
                    self.evidence = {key: value for key, value in self.evidence.items() if key in retained}
                self.latest[zone] = result
            self.history.append({"timestamp": timestamp,
                                 **{zone: result["anomaly_score"] for zone, result in self.latest.items()}})

    def run(self):
        while not self.stop.is_set():
            started = time.monotonic()
            try:
                with self.lock:
                    should_read = not self.error and (not self.paused or self.source.live)
                if should_read:
                    self.tick()
            except Exception as error:
                logging.exception("Audio processing stopped")
                with self.lock:
                    self.error = str(error)
                    for pipeline in self.pipelines.values():
                        pipeline.break_continuity()
            delay = max(0.001, self.source.interval - (time.monotonic() - started))
            self.stop.wait(delay)

    def start(self):
        self.thread = threading.Thread(target=self.run, name="audio-capture", daemon=True)
        self.thread.start()

    def close(self):
        self.stop.set()
        if self.thread:
            self.thread.join(timeout=2)
        if not self.thread or not self.thread.is_alive():
            self.source.close()

    def state(self):
        with self.lock:
            return {"enabled": True, "source": self.source.name,
                    "synthetic": self.source.synthetic, "demo": isinstance(self.source, DemoAudioSource),
                    "scenario": getattr(self.source, "scenario", None), "paused": self.paused,
                    "error": self.error, "sample_rate": self.source.sample_rate,
                    "window_seconds": WINDOW_SECONDS, "zones": dict(self.latest),
                    "history": list(self.history), "events": list(self.events),
                    "conveyor": self.conveyor.state(), "labels": LABELS,
                    "classifier": "Frozen spectral baseline · untrained"}

    def control(self, action, body):
        with self.lock:
            if action == "pause":
                if not isinstance(body.get("paused"), bool):
                    raise ValueError("paused must be a boolean")
                if self.paused != body["paused"]:
                    self.samples.clear()
                    for pipeline in self.pipelines.values():
                        pipeline.break_continuity()
                        if pipeline.baseline is None:
                            pipeline.reference.clear()
                self.paused = body["paused"]
            elif action in ("demo", "reset"):
                if action == "demo":
                    if not isinstance(self.source, DemoAudioSource):
                        raise ValueError("Scenario selection is only available for synthetic audio")
                    scenario = body.get("scenario")
                    if scenario not in AUDIO_SCENARIOS:
                        raise ValueError("Unknown audio scenario")
                    self.source = DemoAudioSource(scenario)
                else:
                    self.source.reset()
                self._reset()
            else:
                raise ValueError("Unknown audio action")

    def clip(self, event_id):
        with self.lock:
            if type(event_id) is not int or event_id not in self.evidence:
                raise ValueError("Audio event is no longer retained")
            return self.evidence[event_id]

    def export(self, body):
        with self.lock:
            event_id = body.get("id")
            data = self.clip(event_id)
            label = body.get("label")
            if label not in LABELS:
                raise ValueError("Choose a supported human audio label")
            event = next(e for e in self.events if e["id"] == event_id)
            name = f"audio-{event_id}-{time.time_ns()}"
            metadata = {**event, "predicted_label": event["label"], "label": label,
                        "audio": f"{name}/audio.wav"}
            buffer = io.BytesIO()
            with zipfile.ZipFile(buffer, "w", zipfile.ZIP_DEFLATED) as archive:
                archive.writestr(metadata["audio"], data)
                archive.writestr(f"{name}/sample.json", json.dumps(metadata, indent=2))
            return buffer.getvalue(), name
