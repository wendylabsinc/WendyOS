"""Agent-owned object detection backend. stdin/stdout are a private protocol.

Encoded streams come from the agent's existing producer hubs. The worker has
no camera device access, agent RPC credentials, or notification credentials.
"""

import base64
import ast
from collections import deque
import hashlib
import io
import json
import os
import re
import socket
import sys
import threading
import time


def emit(result):
    line = json.dumps(result, allow_nan=False) + "\n"
    with output_lock:
        channel = protocol or sys.stdout
        channel.write(line)
        channel.flush()


output_lock = threading.Lock()
# The protocol channel: a private copy of the agent's stdout pipe once main()
# has claimed it (accelerator.claim_stdout), so that nothing else can write to
# it. Until then, and in tests, sys.stdout.
protocol = None

# The worker exits, so the agent restarts it with a fresh process, once this
# many of the last FAILURE_WINDOW frames failed. Fewer failures cost only their
# frames. A window rather than a run of consecutive failures, so a detector
# that fails nine frames in ten still counts as broken.
FAILURE_WINDOW = 20
MAX_FAILURES_IN_WINDOW = 10


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


# A Hugging Face commit. The agent accepts no other revision (data/inference.go).
COMMIT = re.compile(r"[0-9a-f]{40}")


class ModelUnavailable(Exception):
    """The pinned model does not load from this device and the Hub cannot be reached."""


def hub_unreachable(exc):
    """Why the Hugging Face Hub cannot be reached, or None when it can.

    A failed connection in the chain of exc says so. Anything else, such as a
    read timeout, needs a connection to the Hub to decide: huggingface_hub 1.5
    retries a failed connection on the client it has just closed, so an
    offline Transformers load ends in a RuntimeError."""
    import httpx
    from urllib.parse import urlsplit
    from huggingface_hub import constants
    seen = set()
    while exc is not None and id(exc) not in seen:
        seen.add(id(exc))
        if isinstance(exc, (httpx.ConnectError, httpx.ConnectTimeout)):
            return f"{type(exc).__name__}: {exc}"
        exc = exc.__cause__ or exc.__context__
    hub = urlsplit(constants.ENDPOINT)
    try:
        addresses = socket.getaddrinfo(hub.hostname, hub.port or 443, type=socket.SOCK_STREAM)
    except OSError as error:
        return f"{hub.hostname}: {error}"
    # One address per family, IPv4 first, 5 s each. huggingface.co has a
    # dozen, so trying them all in turn, as socket.create_connection does,
    # takes a minute on a network that drops packets. IPv4 goes first because
    # a broken IPv6 route is common and would otherwise use up the time.
    first = {}
    for info in addresses:
        first.setdefault(info[0], info)
    last = OSError("no address")
    for family, kind, protocol, _, address in sorted(first.values(), key=lambda info: info[0] != socket.AF_INET):
        try:
            with socket.socket(family, kind, protocol) as connection:
                connection.settimeout(5)
                connection.connect(address)
            return None
        except OSError as error:
            last = error
    return f"{hub.hostname}: {last}"


def revision_cached(config):
    """Whether any file of the pinned revision is in the Hugging Face cache."""
    from huggingface_hub import constants
    from huggingface_hub.file_download import repo_folder_name
    folder = repo_folder_name(repo_id=config["model"], repo_type="model")
    return os.path.isdir(os.path.join(constants.HF_HUB_CACHE, folder, "snapshots", config["revision"]))


def load_pinned(config, load):
    """Returns load(local_files_only): from the local Hugging Face cache alone
    when the pinned revision is there, and from the Hub only when something is
    missing.

    A commit names immutable files, so a cached copy needs no freshness check.
    Before, every start asked huggingface.co about the model, so a camera that
    rebooted without internet stopped detecting, with the model on its disk.
    The trade-off: an optional file the cache lacks, such as one a newer
    transformers starts to read, is not fetched while the model loads without it."""
    name = config["model"] + "@" + config["revision"][:7]
    local = None
    if COMMIT.fullmatch(config["revision"]):
        try:
            return load(True)
        except OSError as exc:
            # Hugging Face raises OSError for a file its cache lacks.
            local = exc
            print(f"model {name} is not complete on this device ({type(exc).__name__}); "
                  "fetching it from Hugging Face", file=sys.stderr, flush=True)
    try:
        return load(False)
    except Exception as exc:
        reason = hub_unreachable(exc)
        if reason is None:
            raise
        where = "is not on this device"
        if local is not None and revision_cached(config):
            # Some of the model is here, so the local error says what to fix,
            # such as a file that is missing or unreadable.
            where = f"did not load from this device ({type(local).__name__}: {str(local)[:200]})"
        raise ModelUnavailable(f"model {name} {where} and the network is "
                               f"unreachable ({reason[:200]})") from exc


class Detector:
    def __init__(self, config):
        import torch
        from transformers import AutoConfig, AutoImageProcessor, AutoModelForObjectDetection, AutoModelForImageClassification

        self.torch = torch
        self.threshold = config["threshold"]
        self.labels = set(config["labels"])
        self.classification = config.get("task") == "image-classification"

        def load(local_files_only):
            options = {"revision": config["revision"], "trust_remote_code": False,
                       "local_files_only": local_files_only}
            processor = AutoImageProcessor.from_pretrained(config["model"], use_fast=False, **options)
            model_config = AutoConfig.from_pretrained(config["model"], **options)
            if hasattr(model_config, "use_pretrained_backbone"):
                model_config.use_pretrained_backbone = False
            factory = AutoModelForImageClassification if self.classification else AutoModelForObjectDetection
            model = factory.from_pretrained(
                config["model"], config=model_config, use_safetensors=True, **options).eval()
            return processor, model

        self.processor, self.model = load_pinned(config, load)
        if not self.labels.issubset(set(self.model.config.id2label.values())):
            raise ValueError("inference.labels contains labels absent from the model")
        if not self.classification and not hasattr(self.processor, "post_process_object_detection"):
            raise ValueError("model processor does not support object detection")
        # The agent offers this backend the CPU alone (inference/accelerator.go).
        import accelerator
        self.runtime = f"PyTorch {torch.__version__}"
        self.accelerator_status = accelerator.plan_status(config.get("accelerator_plan"), self.runtime)
        self.provenance = {"engine": "cpu", "precision": "fp32"}

    def __call__(self, frame):
        image = frame.to_image()
        inputs = self.processor(images=image, return_tensors="pt")
        started = time.perf_counter()
        with self.torch.inference_mode():
            outputs = self.model(**inputs)
        self.provenance["infer_ms"] = round((time.perf_counter() - started) * 1000, 2)
        if self.classification:
            logits = outputs.logits[0]
            scores = logits.sigmoid() if self.model.config.problem_type == "multi_label_classification" else logits.softmax(dim=-1)
            return classification_predictions(scores.tolist(), self.model.config.id2label, self.labels, self.threshold)
        result = self.processor.post_process_object_detection(
            outputs, target_sizes=[(image.height, image.width)], threshold=self.threshold)[0]
        return [{"label": self.model.config.id2label[int(label)], "score": float(score), "box": box.tolist()}
                for label, score, box in zip(result["labels"], result["scores"], result["boxes"])
                if self.model.config.id2label[int(label)] in self.labels][:100]


def classification_predictions(scores, id2label, selected, threshold):
    # Classification describes the whole image. No object box is invented.
    return sorted([{"label": id2label[index], "score": float(score)}
                   for index, score in enumerate(scores)
                   if id2label.get(index) in selected and threshold <= score <= 1],
                  key=lambda item: -item["score"])[:100]


class YOLODetector:
    """YOLOv8/YOLO11 raw detection ONNX, with no repository Python or pickle.

    Preprocessing and decoding are shared Python; an engine sees only the
    letterboxed [1, 3, height, width] tensor. That keeps every engine's output
    comparable with the CPU's, which is how a TensorRT engine is checked
    before it replaces the CPU (accelerator.py)."""

    def __init__(self, config):
        import numpy as np
        import onnx
        import onnxruntime as ort
        from huggingface_hub import get_hf_file_metadata, hf_hub_download, hf_hub_url

        options = {"repo_id": config["model"], "filename": config["model_file"],
                   "revision": config["revision"]}

        def load(local_files_only):
            if local_files_only:
                return hf_hub_download(**options, token=False, local_files_only=True)
            # Check the size before downloading. A cached file is checked below.
            metadata = get_hf_file_metadata(hf_hub_url(**options), token=False)
            if metadata.size is None or not 0 < metadata.size <= 512 << 20:
                raise ValueError("ONNX model must be at most 512 MiB")
            return hf_hub_download(**options, token=False)

        filename = load_pinned(config, load)
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
        self.model_path, self.model_sha256 = filename, hashlib.sha256(model_bytes).hexdigest()
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
        import accelerator
        plan = config.get("accelerator_plan")
        output_shape = [1, len(self.names) + 4, outputs[0].shape[2]]
        if type(output_shape[2]) is not int and plan and "tensorrt" in (plan.get("candidates") or []):
            # A fixed-size engine needs a fixed anchor count.
            plan = dict(plan, candidates=["cpu"], degraded=True,
                        reason="the model's output has no fixed anchor count, so it cannot get a TensorRT engine")
        cpu = accelerator.CpuEngine(self.session, self.input_name, f"ONNX Runtime {ort.__version__}")
        self.accelerator = accelerator.Accelerator(
            plan, cpu,
            {"path": filename, "sha256": self.model_sha256, "input_shape": shape, "output_shape": output_shape,
             "threshold": self.threshold, "decode": self.decode_model_space},
            lambda message: emit(message))
        self.accelerator_status = self.accelerator.status()
        self.provenance = {}

    def decode_model_space(self, output):
        """Detections in the model's own input pixels: for comparing engines."""
        return yolo_detections(output, self.names, self.labels, self.threshold, 1, 0, 0, self.width, self.height)

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
        started = time.perf_counter()
        output, engine = self.accelerator.infer(tensor)
        if output is None:
            # A tensorrt campaign waiting for its engine scores nothing.
            return None
        self.provenance = {"engine": engine.name, "precision": engine.precision,
                           "infer_ms": round((time.perf_counter() - started) * 1000, 2)}
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
                    if not decoder.stream.feed(payload):
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
                frame = decoder.take(time.monotonic(), 1 / config["rate"])
                if frame is None:
                    continue
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
                if detections is None:
                    # No engine is serving yet. Not a failure, and no result.
                    continue
                recent.append(True)
                with lock:
                    # A source reset while inference ran invalidates the result.
                    if decoders.get(decoder.source_id) is not decoder or decoder.stream.stopped:
                        continue
                # Boxes are in pixels of this picture. Its size lets the agent
                # place them on a picture shown at any other size.
                prediction = {"type": "prediction", "source_id": decoder.source_id,
                              "generation": decoder.generation, "detections": detections,
                              "width": frame.width, "height": frame.height}
                # Which engine scored this frame, at what precision, how fast.
                prediction.update(getattr(detector, "provenance", None) or {})
                emit(prediction)
            stopped.wait(0.02)
    finally:
        with lock:
            for decoder in decoders.values():
                decoder.stop()


def main():
    # First of all, before onnxruntime, PyTorch, TensorRT or CUDA is loaded:
    # from here on, whatever native code prints to stdout lands on stderr.
    # That includes TensorRT and CUDA, which the accelerator loads in the
    # background long after ready, and only when an engine is about to load.
    import accelerator
    global protocol
    protocol = accelerator.claim_stdout()
    config = json.loads(sys.stdin.buffer.readline())
    try:
        detector = YOLODetector(config) if config.get("backend") == "yolo_onnx" else Detector(config)
    except ModelUnavailable as exc:
        # The agent shows this reason as the campaign's inference error.
        emit({"type": "error", "error": str(exc)})
        sys.exit(1)
    # Ready as soon as the CPU can score. A TensorRT engine, found or built in
    # the background, takes over later, so its build never risks the agent's
    # ready deadline.
    emit({"type": "ready", "accelerator": getattr(detector, "accelerator_status", None)})
    accelerator = getattr(detector, "accelerator", None)
    if accelerator is not None:
        accelerator.start()
    try:
        run(config, detector)
    finally:
        if accelerator is not None:
            accelerator.stop()


if __name__ == "__main__":
    main()
