"""Agent-owned object detection backend. stdin/stdout are a private protocol.

Encoded streams come from the agent's existing producer hubs. The worker has
no camera device access, agent RPC credentials, or notification credentials.
"""

import base64
from collections import deque
import io
import json
import sys
import threading
import time


def emit(result):
    with output_lock:
        print(json.dumps(result, allow_nan=False), flush=True)


output_lock = threading.Lock()


class StreamBytes(io.RawIOBase):
    """Bounded streaming file for libav, including H.264 and VP8/WebM."""

    def __init__(self):
        super().__init__()
        self.condition = threading.Condition()
        self.chunks = deque()
        self.pending = 0
        self.stopped = False

    def readable(self):
        return True

    def seekable(self):
        return False

    def feed(self, payload):
        with self.condition:
            if self.stopped or self.pending + len(payload) > 8 << 20:
                return False
            self.chunks.append(payload)
            self.pending += len(payload)
            self.condition.notify()
            return True

    def read(self, size=-1):
        if size == 0:
            return b""
        with self.condition:
            while not self.chunks and not self.stopped:
                self.condition.wait()
            if self.stopped:
                return b""
            chunk = self.chunks.popleft()
            if 0 < size < len(chunk):
                self.chunks.appendleft(chunk[size:])
                chunk = chunk[:size]
            self.pending -= len(chunk)
            return chunk

    def stop(self):
        with self.condition:
            self.stopped = True
            self.chunks.clear()
            self.pending = 0
            self.condition.notify_all()


class Decoder:
    def __init__(self, source_id, generation, encoding):
        self.source_id, self.generation, self.encoding = source_id, generation, encoding
        self.stream = StreamBytes()
        self.lock = threading.Lock()
        self.latest = None
        self.last_scored = 0
        self.thread = threading.Thread(target=self.decode, daemon=True)
        self.thread.start()

    def decode(self):
        import av

        try:
            # Probe the actual bytes: VP8 producers deliver a WebM container,
            # while H.264 producers deliver Annex B. No frame-rate guessing.
            with av.open(self.stream, mode="r") as container:
                for frame in container.decode(video=0):
                    with self.lock:
                        if self.stream.stopped:
                            break
                        self.latest = (time.monotonic(), frame)
        except Exception as exc:
            if not self.stream.stopped:
                emit({"type": "source_error", "source_id": self.source_id,
                      "generation": self.generation, "error": "decode: " + type(exc).__name__})
        finally:
            self.stream.stop()

    def take(self, now, interval):
        with self.lock:
            if self.stream.stopped or now - self.last_scored < interval:
                return None
            latest, self.latest = self.latest, None
            if latest is None or now - latest[0] > 5:
                return None
            self.last_scored = now
            return latest[1]

    def stop(self):
        self.stream.stop()
        with self.lock:
            self.latest = None


class Detector:
    def __init__(self, config):
        import torch
        from transformers import AutoConfig, AutoImageProcessor, AutoModelForObjectDetection

        self.torch = torch
        self.threshold = config["threshold"]
        self.labels = set(config["labels"])
        options = {"revision": config["revision"], "trust_remote_code": False}
        self.processor = AutoImageProcessor.from_pretrained(config["model"], use_fast=False, **options)
        model_config = AutoConfig.from_pretrained(config["model"], **options)
        if hasattr(model_config, "use_pretrained_backbone"):
            model_config.use_pretrained_backbone = False
        self.model = AutoModelForObjectDetection.from_pretrained(
            config["model"], config=model_config, use_safetensors=True, **options).eval()
        if not self.labels.issubset(set(self.model.config.id2label.values())):
            raise ValueError("inference.labels contains labels absent from the model")
        if not hasattr(self.processor, "post_process_object_detection"):
            raise ValueError("model processor does not support object detection")

    def __call__(self, frame):
        image = frame.to_image()
        inputs = self.processor(images=image, return_tensors="pt")
        with self.torch.inference_mode():
            outputs = self.model(**inputs)
        result = self.processor.post_process_object_detection(
            outputs, target_sizes=[(image.height, image.width)], threshold=self.threshold)[0]
        return [{"label": self.model.config.id2label[int(label)], "score": float(score), "box": box.tolist()}
                for label, score, box in zip(result["labels"], result["scores"], result["boxes"])
                if self.model.config.id2label[int(label)] in self.labels][:100]


def run(config, detector):
    lock = threading.Lock()
    decoders = {}
    stopped = threading.Event()

    def receive():
        try:
            while line := sys.stdin.buffer.readline(16 << 20):
                item = json.loads(line)
                source_id, generation = item["source_id"], item["generation"]
                with lock:
                    decoder = decoders.get(source_id)
                    # A late close from a retired subscription cannot close its replacement.
                    if item.get("end"):
                        if decoder and decoder.generation == generation:
                            decoders.pop(source_id).stop()
                        continue
                    payload = base64.b64decode(item["payload"], validate=True)
                    encoding = item["encoding"]
                    if encoding not in ("h264", "vp8"):
                        if decoder:
                            decoders.pop(source_id).stop()
                        emit({"type": "source_error", "source_id": source_id,
                              "generation": generation, "error": "unsupported camera encoding"})
                        continue
                    reset = decoder and (decoder.generation != generation or decoder.encoding != encoding
                                         or decoder.stream.stopped or item.get("dropped_before", 0))
                    if reset:
                        decoder.stop()
                        decoder = None
                    if decoder is None:
                        decoder = Decoder(source_id, generation, encoding)
                        decoders[source_id] = decoder
                    if not decoder.stream.feed(payload):
                        # Never splice an encoded stream after losing bytes. The next
                        # sample starts a new decoder and waits for a random-access unit.
                        decoder.stop()
                        emit({"type": "source_error", "source_id": source_id,
                              "generation": generation, "error": "decoder queue overflow; resynchronizing"})
        finally:
            stopped.set()

    threading.Thread(target=receive, daemon=True).start()
    try:
        while not stopped.is_set():
            with lock:
                current = list(decoders.values())
            for decoder in current:
                frame = decoder.take(time.monotonic(), 1 / config["rate"])
                if frame is None:
                    continue
                detections = detector(frame)
                with lock:
                    # A source reset while inference ran invalidates the result.
                    if decoders.get(decoder.source_id) is not decoder or decoder.stream.stopped:
                        continue
                emit({"type": "prediction", "source_id": decoder.source_id,
                      "generation": decoder.generation, "detections": detections})
            stopped.wait(0.02)
    finally:
        with lock:
            for decoder in decoders.values():
                decoder.stop()


def main():
    config = json.loads(sys.stdin.buffer.readline())
    detector = Detector(config)
    emit({"type": "ready"})
    run(config, detector)


if __name__ == "__main__":
    main()
