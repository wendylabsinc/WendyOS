"""Agent-owned object detection backend. stdin/stdout are a private protocol.

Encoded streams come from the agent's existing producer hubs. The worker has
no camera device access, agent RPC credentials, or notification credentials.
"""

import base64
import ast
from collections import deque
import io
import json
import os
import sys
import threading
import time


def emit(result):
    with output_lock:
        print(json.dumps(result, allow_nan=False), flush=True)


output_lock = threading.Lock()

# The worker exits, so the agent restarts it with a fresh process, once this
# many of the last FAILURE_WINDOW frames failed. Fewer failures cost only their
# frames. A window rather than a run of consecutive failures, so a detector
# that fails nine frames in ten still counts as broken.
FAILURE_WINDOW = 20
MAX_FAILURES_IN_WINDOW = 10

# How many fed chunks a stream remembers the samples of, at most. It is a
# bound for a decoder that reports no packet positions, which never lets any
# go; more than the input queue ever holds at camera chunk sizes.
MAX_SAMPLE_MARKS = 4096
# How far frame reordering may move a picture from its packet, in packets.
# H.264 holds at most 16 pictures back.
MAX_REORDER = 64


def detection_error(exc):
    """A detector failure for one frame, cut to 512 UTF-8 bytes."""
    message = "inference: " + type(exc).__name__
    try:
        text = str(exc)
    except Exception:
        text = ""
    if text:
        message += ": " + text
    return message.encode(errors="replace")[:512].decode(errors="ignore")


class StreamBytes(io.RawIOBase):
    """Bounded streaming file for libav, including H.264 and VP8/WebM."""

    def __init__(self):
        super().__init__()
        self.condition = threading.Condition()
        self.chunks = deque()
        self.pending = 0
        self.stopped = False
        # Byte offset of the next chunk from the start of the stream, the
        # position libav reports for a packet, and (offset, sample) of each
        # chunk fed, oldest first.
        self.fed = 0
        self.marks = deque(maxlen=MAX_SAMPLE_MARKS)

    def readable(self):
        return True

    def seekable(self):
        return False

    def feed(self, payload, sample=None):
        with self.condition:
            if self.stopped or self.pending + len(payload) > 8 << 20:
                return False
            self.marks.append((self.fed, sample))
            self.fed += len(payload)
            self.chunks.append(payload)
            self.pending += len(payload)
            self.condition.notify()
            return True

    def sample_at(self, position):
        """The sample of the chunk that holds position, a byte offset libav
        reported for a packet, or None. Packet positions only grow, so the
        chunks before this one are forgotten."""
        with self.condition:
            marks = self.marks
            while len(marks) > 1 and marks[1][0] <= position:
                marks.popleft()
            if marks and marks[0][0] <= position:
                return marks[0][1]
            return None

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
            self.marks.clear()
            self.pending = 0
            self.condition.notify_all()


def decoded_pictures(container, stream):
    """Decodes container's video, yielding each picture with the sample of the
    chunk that brought its first byte, or None when that is unknown.

    A packet's pts names the packet, and libav carries pts through decoding,
    frame reordering included, so a picture's pts says which packet started
    it. The packet's byte position says which chunk that was."""
    recent = [None] * MAX_REORDER
    count = 0
    for packet in container.demux(video=0):
        if packet.size:
            count += 1
            recent[count % MAX_REORDER] = None if packet.pos is None else stream.sample_at(packet.pos)
            packet.pts = count
        for frame in packet.decode():
            pts = frame.pts
            known = pts is not None and 0 <= count - pts < MAX_REORDER
            yield frame, recent[pts % MAX_REORDER] if known else None


class Decoder:
    def __init__(self, source_id, generation, encoding, initialization=b""):
        self.source_id, self.generation, self.encoding = source_id, generation, encoding
        self.stream = StreamBytes()
        if initialization:
            self.stream.feed(initialization)
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
                for frame, sample in decoded_pictures(container, self.stream):
                    with self.lock:
                        if self.stream.stopped:
                            break
                        self.latest = (time.monotonic(), frame, sample)
        except Exception as exc:
            if not self.stream.stopped:
                emit({"type": "source_error", "source_id": self.source_id,
                      "generation": self.generation, "error": "decode: " + type(exc).__name__})
        finally:
            self.stream.stop()

    def take(self, now, interval):
        """The newest decoded picture and the sample it came in, or None."""
        with self.lock:
            if self.stream.stopped or now - self.last_scored < interval:
                return None
            latest, self.latest = self.latest, None
            if latest is None or now - latest[0] > 5:
                return None
            self.last_scored = now
            return latest[1], latest[2]

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


class YOLODetector:
    """YOLOv8/YOLO11 raw detection ONNX, with no repository Python or pickle."""

    def __init__(self, config):
        import numpy as np
        import onnx
        import onnxruntime as ort
        from huggingface_hub import get_hf_file_metadata, hf_hub_download, hf_hub_url

        options = {"repo_id": config["model"], "filename": config["model_file"],
                   "revision": config["revision"]}
        metadata = get_hf_file_metadata(hf_hub_url(**options), token=False)
        if metadata.size is None or not 0 < metadata.size <= 512 << 20:
            raise ValueError("ONNX model must be at most 512 MiB")
        filename = hf_hub_download(**options, token=False)
        if not 0 < os.path.getsize(filename) <= 512 << 20:
            raise ValueError("ONNX model must be at most 512 MiB")
        # Inspect without loading external tensors. Reject references anywhere
        # in the graph, including attributes, subgraphs and local functions.
        with open(filename, "rb") as model:
            model_bytes = model.read((512 << 20) + 1)
        if len(model_bytes) > 512 << 20:
            raise ValueError("ONNX model must be at most 512 MiB")
        def reject_external_tensors(message):
            if isinstance(message, onnx.TensorProto):
                if message.data_location == onnx.TensorProto.EXTERNAL or message.external_data:
                    raise ValueError("YOLO ONNX external tensor files are not supported")
            for field, value in message.ListFields():
                if field.message_type is not None:
                    for child in value if field.is_repeated else [value]:
                        reject_external_tensors(child)
        reject_external_tensors(onnx.load_model_from_string(model_bytes))
        session_options = ort.SessionOptions()
        session_options.intra_op_num_threads = 2
        session_options.inter_op_num_threads = 1
        self.session = ort.InferenceSession(model_bytes, sess_options=session_options,
                                           providers=["CPUExecutionProvider"])
        inputs, outputs = self.session.get_inputs(), self.session.get_outputs()
        if len(inputs) != 1 or inputs[0].type != "tensor(float)":
            raise ValueError("YOLO ONNX requires one float32 image input")
        shape = inputs[0].shape
        if (len(shape) != 4 or shape[:2] != [1, 3]
                or any(type(n) is not int or not 32 <= n <= 1280 for n in shape[2:])):
            raise ValueError("YOLO ONNX requires static input [1, 3, height, width], 32..1280 pixels")
        metadata = self.session.get_modelmeta().custom_metadata_map
        if metadata.get("task", "detect") != "detect":
            raise ValueError("YOLO ONNX must be a detection model")
        raw_names = metadata.get("names", "")
        if not raw_names or len(raw_names) > 128 << 10:
            raise ValueError("YOLO ONNX must embed its class names")
        names = ast.literal_eval(raw_names)
        if isinstance(names, list):
            names = dict(enumerate(names))
        if not isinstance(names, dict) or not 1 <= len(names) <= 1000:
            raise ValueError("Invalid YOLO class names")
        self.names = [names.get(n, names.get(str(n))) for n in range(len(names))]
        if any(not isinstance(n, str) or not n or len(n) > 128 for n in self.names):
            raise ValueError("YOLO class names must use consecutive class IDs")
        if len(set(self.names)) != len(self.names):
            raise ValueError("YOLO class names must be unique")
        self.labels, self.threshold = set(config["labels"]), config["threshold"]
        if not self.labels.issubset(set(self.names)):
            raise ValueError("inference.labels contains labels absent from the model")
        if (len(outputs) != 1 or len(outputs[0].shape) != 3 or outputs[0].shape[0] != 1
                or outputs[0].shape[1] != len(self.names) + 4):
            raise ValueError("Expected raw YOLOv8/YOLO11 detection output [1, 4 + classes, anchors]; export without NMS")
        self.np, self.input_name = np, inputs[0].name
        self.height, self.width = shape[2:]

    def __call__(self, frame):
        from PIL import Image
        np = self.np
        image = frame.to_image().convert("RGB")
        scale = min(self.width / image.width, self.height / image.height)
        size = max(1, round(image.width * scale)), max(1, round(image.height * scale))
        dx, dy = (self.width - size[0]) // 2, (self.height - size[1]) // 2
        canvas = Image.new("RGB", (self.width, self.height), (114, 114, 114))
        canvas.paste(image.resize(size, Image.Resampling.BILINEAR), (dx, dy))
        tensor = np.asarray(canvas, dtype=np.float32).transpose(2, 0, 1)[None] / 255.0
        output = self.session.run(None, {self.input_name: tensor})[0]
        return yolo_detections(output, self.names, self.labels, self.threshold,
                               scale, dx, dy, image.width, image.height)


def yolo_detections(output, names, labels, threshold, scale, dx, dy, width, height):
    """Decode raw boxes with class-aware NMS in original-frame coordinates."""
    import numpy as np
    if (output.ndim != 3 or output.shape[:2] != (1, len(names) + 4)
            or output.shape[2] > 100000 or not np.isfinite(output).all()):
        raise ValueError("Invalid YOLO detection tensor")
    rows = output[0].T
    classes = rows[:, 4:].argmax(axis=1)
    scores = rows[np.arange(len(rows)), classes + 4]
    keep = ((scores >= threshold) & (scores <= 1)
            & np.isin(classes, [n for n, name in enumerate(names) if name in labels])
            & (rows[:, 2] > 0) & (rows[:, 3] > 0))
    rows, classes, scores = rows[keep], classes[keep], scores[keep]
    if not len(rows):
        return []
    boxes = np.column_stack(((rows[:, 0] - rows[:, 2] / 2 - dx) / scale,
                             (rows[:, 1] - rows[:, 3] / 2 - dy) / scale,
                             (rows[:, 0] + rows[:, 2] / 2 - dx) / scale,
                             (rows[:, 1] + rows[:, 3] / 2 - dy) / scale))
    boxes[:, (0, 2)] = boxes[:, (0, 2)].clip(0, width)
    boxes[:, (1, 3)] = boxes[:, (1, 3)].clip(0, height)
    areas = (boxes[:, 2] - boxes[:, 0]) * (boxes[:, 3] - boxes[:, 1])
    order = np.argsort(-scores)[:1000]
    detections = []
    while len(order) and len(detections) < 100:
        i, rest = order[0], order[1:]
        if areas[i] <= 0:
            order = rest
            continue
        detections.append({"label": names[int(classes[i])], "score": float(scores[i]),
                           "box": boxes[i].tolist()})
        intersection = np.maximum(0, np.minimum(boxes[i, 2:], boxes[rest, 2:])
                                  - np.maximum(boxes[i, :2], boxes[rest, :2])).prod(axis=1)
        union = areas[i] + areas[rest] - intersection
        iou = np.divide(intersection, union, out=np.zeros_like(intersection), where=union > 0)
        order = rest[(classes[rest] != classes[i]) | (iou <= 0.45)]
    return detections


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
                        initialization = base64.b64decode(item.get("initialization", ""), validate=True)
                        if len(initialization) > 1 << 20:
                            raise ValueError("WebM initialization exceeds 1MiB")
                        # Initial subscribers already receive the EBML header in
                        # the payload. Late joins and decoder resets need the
                        # producer's cached header before libav can resynchronize.
                        if encoding != "vp8" or payload.startswith(b"\x1a\x45\xdf\xa3"):
                            initialization = b""
                        decoder = Decoder(source_id, generation, encoding, initialization)
                        decoders[source_id] = decoder
                    if not decoder.stream.feed(payload, item.get("sample")):
                        # Never splice an encoded stream after losing bytes. The next
                        # sample starts a new decoder and waits for a random-access unit.
                        decoder.stop()
                        emit({"type": "source_error", "source_id": source_id,
                              "generation": generation, "error": "decoder queue overflow; resynchronizing"})
        finally:
            stopped.set()

    threading.Thread(target=receive, daemon=True).start()
    recent = deque(maxlen=FAILURE_WINDOW)
    try:
        while not stopped.is_set():
            with lock:
                current = list(decoders.values())
            for decoder in current:
                taken = decoder.take(time.monotonic(), 1 / config["rate"])
                if taken is None:
                    continue
                frame, sample = taken
                try:
                    detections = detector(frame)
                except Exception as exc:
                    # One bad frame costs that frame, not the worker. Before,
                    # the exception ended the process, and the agent lost the
                    # stderr tail once the model had loaded.
                    recent.append(False)
                    if recent.count(False) >= MAX_FAILURES_IN_WINDOW:
                        emit({"type": "error", "error": detection_error(exc)})
                        return
                    emit({"type": "source_error", "source_id": decoder.source_id,
                          "generation": decoder.generation, "error": detection_error(exc)})
                    continue
                recent.append(True)
                with lock:
                    # A source reset while inference ran invalidates the result.
                    if decoders.get(decoder.source_id) is not decoder or decoder.stream.stopped:
                        continue
                # Boxes are in pixels of this picture. Its size and its sample
                # let the agent place them on the video viewers see.
                prediction = {"type": "prediction", "source_id": decoder.source_id,
                              "generation": decoder.generation, "detections": detections,
                              "width": frame.width, "height": frame.height}
                if sample is not None:
                    prediction["sample"] = sample
                emit(prediction)
            stopped.wait(0.02)
    finally:
        with lock:
            for decoder in decoders.values():
                decoder.stop()


def main():
    config = json.loads(sys.stdin.buffer.readline())
    detector = YOLODetector(config) if config.get("backend") == "yolo_onnx" else Detector(config)
    emit({"type": "ready"})
    run(config, detector)


if __name__ == "__main__":
    main()
