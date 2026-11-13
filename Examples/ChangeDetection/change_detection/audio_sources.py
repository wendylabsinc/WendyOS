"""Bounded PCM inputs. Stereo WAVs map left to hopper, right to conveyor."""

import queue
import wave

import numpy as np

from .audio import SAMPLE_RATE, WINDOW_SECONDS, ZONES

AUDIO_SCENARIOS = ("normal", "metal_impact", "belt_rip", "unknown", "silence", "clipping")


class DemoAudioSource:
    name = "Synthetic hopper + conveyor microphones"
    synthetic = True
    live = False
    sample_rate = SAMPLE_RATE
    interval = WINDOW_SECONDS
    zones = ZONES

    def __init__(self, scenario="metal_impact", seed=19):
        if scenario not in AUDIO_SCENARIOS:
            raise ValueError("Unknown audio scenario")
        self.scenario = scenario
        self.seed = seed
        self.reset()

    def reset(self):
        self.position = 0
        self.rng = np.random.default_rng(self.seed)

    def read(self):
        count = round(self.sample_rate * self.interval)
        t = (self.position + np.arange(count)) / self.sample_rate
        local = np.arange(count) / self.sample_rate
        channels = []
        for zone in self.zones:
            normal = 0.055 * np.sin(2 * np.pi * 120 * t) + 0.025 * np.sin(2 * np.pi * 280 * t)
            normal += self.rng.normal(0, 0.0015, count)
            # Known-normal pre-roll; all inference sees is the resulting PCM.
            if 5 <= t[0] < 9:
                if self.scenario == "metal_impact" and zone == "hopper":
                    normal += np.exp(-local * 9) * (0.55 * np.sin(2 * np.pi * 2400 * t)
                                                  + 0.22 * np.sin(2 * np.pi * 3700 * t))
                elif self.scenario == "belt_rip" and zone == "conveyor":
                    noise = self.rng.normal(0, 0.13, count + 1)
                    normal += np.diff(noise)
                elif self.scenario == "unknown":
                    normal += 0.55 * np.sin(2 * np.pi * 550 * t)
                elif self.scenario == "silence":
                    normal[:] = 0
                elif self.scenario == "clipping":
                    normal = np.sign(np.sin(2 * np.pi * 120 * t))
            channels.append(np.clip(normal, -1, 1))
        self.position += count
        return np.column_stack(channels).astype(np.float32), self.position / self.sample_rate

    def close(self):
        pass


class WavAudioSource:
    name = "PCM WAV recording"
    synthetic = False
    live = False
    interval = WINDOW_SECONDS

    def __init__(self, path, zone="hopper"):
        self.reader = wave.open(str(path), "rb")
        try:
            self.sample_rate = self.reader.getframerate()
            channels = self.reader.getnchannels()
            if self.reader.getsampwidth() != 2 or channels not in (1, 2) or not 8000 <= self.sample_rate <= 96000:
                raise ValueError("WAV input must be 16-bit PCM, mono or stereo, at 8–96 kHz")
            if zone not in ZONES:
                raise ValueError("Unknown audio zone")
            self.zones = ZONES if channels == 2 else (zone,)
        except Exception:
            self.reader.close()
            raise

    def read(self):
        count = round(self.sample_rate * self.interval)
        raw = self.reader.readframes(count)
        if len(raw) != count * len(self.zones) * 2:
            raise EOFError("Audio recording ended; incomplete final window is not analyzed")
        samples = np.frombuffer(raw, dtype="<i2").reshape(-1, len(self.zones)).astype(np.float32) / 32768
        return samples, self.reader.tell() / self.sample_rate

    def reset(self):
        self.reader.rewind()

    def close(self):
        self.reader.close()


class MicrophoneSource:
    name = "Live microphone"
    synthetic = False
    live = True
    sample_rate = SAMPLE_RATE
    interval = WINDOW_SECONDS

    def __init__(self, device=None, zone="hopper"):
        if zone not in ZONES:
            raise ValueError("Unknown audio zone")
        try:
            import sounddevice as sd
        except ImportError as error:
            raise ValueError("Microphone input requires requirements-audio.txt and PortAudio") from error
        self.zones = (zone,)
        self.blocks = queue.Queue(maxsize=8)
        self.error = None
        self.origin = None
        self.stream = sd.InputStream(device=device, samplerate=self.sample_rate, channels=1,
                                     dtype="float32", blocksize=round(self.sample_rate * self.interval),
                                     callback=self._capture)
        try:
            self.stream.start()
        except Exception:
            self.stream.close()
            raise

    def _capture(self, data, frames, timing, status):
        if status:
            self.error = f"Microphone capture failed: {status}"
            return
        if self.origin is None:
            self.origin = timing.inputBufferAdcTime
        timestamp = timing.inputBufferAdcTime - self.origin + frames / self.sample_rate
        try:
            self.blocks.put_nowait((data.copy(), timestamp))
        except queue.Full:
            self.error = "Microphone queue overflow; restart audio to discard stale data"

    def read(self):
        if self.error:
            raise OSError(self.error)
        try:
            result = self.blocks.get(timeout=1)
        except queue.Empty as error:
            raise OSError("Microphone stopped returning samples") from error
        if self.error:
            raise OSError(self.error)
        return result

    def reset(self):
        while True:
            try:
                self.blocks.get_nowait()
            except queue.Empty:
                break
        self.error = None

    def close(self):
        self.stream.close()
