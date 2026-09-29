"""Agent-owned world view perception worker. stdin/stdout are a private protocol.

The worker proposes and measures candidate objects; it never classifies them.
Matching against campaign object descriptors, fusion and tracking happen in the
agent. Encoded streams come from the agent's existing producer hubs. The worker
has no camera device access, agent Remote Procedure Call (RPC) credentials, or
notification credentials.

StreamBytes and Decoder are copied from the inference worker on purpose: each
worker is a separate, independently locked asset.
"""

import base64
from collections import deque
import io
import json
import math
import sys
import threading
import time

import cv2
import numpy as np

PROPOSERS = ("contours",)
MAX_SIDE = 640
PALETTE_SAMPLES = 4096
RATE_WINDOW_SECONDS = 5.0
MIN_PAIR_PERIOD_SECONDS = 0.1
MAX_DEPTH_SIDE = 8192


def emit(result):
    with output_lock:
        print(json.dumps(result, allow_nan=False), flush=True)


output_lock = threading.Lock()


class StreamBytes(io.RawIOBase):
    """Bounded streaming file for libav, including H.264 and VP8/WebM.

    Each chunk carries the metadata of the sample it came from; read_meta is
    the metadata of the chunk libav read most recently.
    """

    def __init__(self):
        super().__init__()
        self.condition = threading.Condition()
        self.chunks = deque()
        self.pending = 0
        self.stopped = False
        self.read_meta = None

    def readable(self):
        return True

    def seekable(self):
        return False

    def feed(self, payload, meta=None):
        with self.condition:
            if self.stopped or self.pending + len(payload) > 8 << 20:
                return False
            self.chunks.append((payload, meta))
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
            chunk, meta = self.chunks.popleft()
            if 0 < size < len(chunk):
                self.chunks.appendleft((chunk[size:], meta))
                chunk = chunk[:size]
            self.pending -= len(chunk)
            if meta is not None:
                self.read_meta = meta
            return chunk

    def stop(self):
        with self.condition:
            self.stopped = True
            self.chunks.clear()
            self.pending = 0
            self.condition.notify_all()


class FrameSlot:
    """Selects which decoded frame the scorer takes next.

    With a rate, only the latest decoded frame is kept and it is released at
    most once per interval. With every_frames, only every Nth decoded frame is
    offered; a slow scorer still sees only the newest offered frame.
    """

    def __init__(self, rate=None, every_frames=None):
        self.interval = 1.0 / rate if rate else 0.0
        self.every = every_frames or 1
        self.count = 0
        self.latest = None
        self.last_scored = -math.inf

    def put(self, now, frame, meta=None):
        self.count += 1
        if self.count % self.every == 0:
            self.latest = (now, frame, meta)

    def take(self, now):
        if now - self.last_scored < self.interval:
            return None
        latest, self.latest = self.latest, None
        if latest is None or now - latest[0] > 5:
            return None
        self.last_scored = now
        return latest[1], latest[2]


class RateMeter:
    """Scored frames per second over a sliding window."""

    def __init__(self, window=RATE_WINDOW_SECONDS):
        self.window = window
        self.times = deque()

    def tick(self, now):
        self.times.append(now)
        while now - self.times[0] > self.window:
            self.times.popleft()
        span = self.times[-1] - self.times[0]
        if len(self.times) < 2 or span <= 0:
            return 0.0
        return (len(self.times) - 1) / span


class Decoder:
    def __init__(self, source_id, generation, encoding, initialization=b"", slot=None):
        self.source_id, self.generation, self.encoding = source_id, generation, encoding
        self.stream = StreamBytes()
        if initialization:
            self.stream.feed(initialization)
        self.lock = threading.Lock()
        self.slot = slot or FrameSlot(rate=1.0)
        self.meter = RateMeter()
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
                        self.slot.put(time.monotonic(), frame, self.stream.read_meta)
        except Exception as exc:
            if not self.stream.stopped:
                emit({"type": "source_error", "source_id": self.source_id,
                      "generation": self.generation, "error": "decode: " + type(exc).__name__})
        finally:
            self.stream.stop()

    def take(self, now):
        with self.lock:
            if self.stream.stopped:
                return None
            return self.slot.take(now)

    def stop(self):
        self.stream.stop()
        with self.lock:
            self.slot.latest = None


class DepthFrame:
    __slots__ = ("generation", "boot_nanos", "pixels")

    def __init__(self, generation, boot_nanos, pixels):
        self.generation, self.boot_nanos, self.pixels = generation, boot_nanos, pixels


def parse_z16(payload, width, height):
    """Raw little-endian uint16 depth, row-major."""
    if not (isinstance(width, int) and isinstance(height, int)
            and 0 < width <= MAX_DEPTH_SIDE and 0 < height <= MAX_DEPTH_SIDE):
        raise ValueError("invalid depth dimensions")
    if len(payload) != width * height * 2:
        raise ValueError("depth payload size does not match dimensions")
    return np.frombuffer(payload, dtype="<u2").reshape(height, width)


class Candidate:
    """One proposal. box is in decoded-frame pixels; mask is a crop of the
    processing-resolution mask covering proc_box."""

    __slots__ = ("box", "area_px", "palette", "silhouette", "proc_box", "mask", "scale")

    def wire(self, metric=None):
        return {"box": self.box, "area_px": self.area_px, "palette": self.palette,
                "silhouette": self.silhouette, "metric": metric}


def cielab(lab8):
    """OpenCV 8-bit Lab to CIELAB: L in [0, 100], a and b signed."""
    lab8 = np.asarray(lab8, dtype=np.float64)
    return [lab8[0] * 100.0 / 255.0, lab8[1] - 128.0, lab8[2] - 128.0]


def palette(lab_pixels):
    """k-means palette (k = min(3, distinct colours)), largest share first."""
    if len(lab_pixels) > PALETTE_SAMPLES:
        lab_pixels = lab_pixels[::math.ceil(len(lab_pixels) / PALETTE_SAMPLES)]
    k = min(3, len(np.unique(lab_pixels, axis=0)))
    if k == 0:
        return []
    cv2.setRNGSeed(0)
    criteria = (cv2.TERM_CRITERIA_EPS + cv2.TERM_CRITERIA_MAX_ITER, 20, 0.5)
    _, labels, centers = cv2.kmeans(lab_pixels.astype(np.float32), k, None, criteria, 3, cv2.KMEANS_PP_CENTERS)
    counts = np.bincount(labels.ravel(), minlength=k)
    total = float(counts.sum())
    entries = [{"lab": [round(float(v), 2) for v in cielab(centers[i])], "share": round(float(counts[i]) / total, 4)}
               for i in np.argsort(-counts, kind="stable") if counts[i] > 0]
    return entries


def _parallel(u, v, tolerance_deg=10.0):
    nu, nv = np.linalg.norm(u), np.linalg.norm(v)
    if nu == 0 or nv == 0:
        return False
    cosine = abs(float(np.dot(u, v))) / (nu * nv)
    return cosine >= math.cos(math.radians(tolerance_deg))


def silhouette(contour):
    """2D primitive and aspect (height over width in frame orientation)."""
    area = cv2.contourArea(contour)
    perimeter = cv2.arcLength(contour, True)
    approx = cv2.approxPolyDP(contour, 0.02 * perimeter, True)
    primitive = "other"
    if len(approx) == 4 and cv2.isContourConvex(approx):
        p = approx.reshape(4, 2).astype(np.float64)
        sides = [p[(i + 1) % 4] - p[i] for i in range(4)]
        pair0, pair1 = _parallel(sides[0], sides[2]), _parallel(sides[1], sides[3])
        if pair0 and pair1:
            primitive = "rect"
        elif pair0 or pair1:
            a, b = (sides[0], sides[2]) if pair0 else (sides[1], sides[3])
            la, lb = np.linalg.norm(a), np.linalg.norm(b)
            if min(la, lb) < 0.9 * max(la, lb):
                primitive = "trapezoid"
    elif perimeter > 0 and 4 * math.pi * area / (perimeter * perimeter) > 0.8:
        primitive = "disc"
    points = cv2.boxPoints(cv2.minAreaRect(contour))
    first, second = points[1] - points[0], points[2] - points[1]
    if abs(first[0]) < abs(first[1]):
        first, second = second, first
    width, height = float(np.linalg.norm(first)), float(np.linalg.norm(second))
    return {"primitive": primitive, "aspect": round(height / width, 4) if width > 0 else 0.0}


def propose(bgr, max_proposals=50, min_area_fraction=0.002, max_side=MAX_SIDE):
    """contours proposer, version 1: colour edges, closed, external contours."""
    frame_h, frame_w = bgr.shape[:2]
    scale = min(1.0, max_side / max(frame_w, frame_h))
    proc = bgr
    if scale < 1.0:
        proc = cv2.resize(bgr, (max(1, round(frame_w * scale)), max(1, round(frame_h * scale))),
                          interpolation=cv2.INTER_AREA)
    lab = cv2.cvtColor(proc, cv2.COLOR_BGR2LAB)
    blurred = cv2.GaussianBlur(lab, (3, 3), 0)
    # Edges on every Lab channel, not only L: equal-lightness colours (red on
    # mid grey) differ only in a and b.
    edges = cv2.Canny(blurred[:, :, 0], 40, 120)
    for channel in (1, 2):
        edges |= cv2.Canny(blurred[:, :, channel], 40, 120)
    kernel = cv2.getStructuringElement(cv2.MORPH_ELLIPSE, (5, 5))
    closed = cv2.morphologyEx(edges, cv2.MORPH_CLOSE, kernel)
    contours, _ = cv2.findContours(closed, cv2.RETR_EXTERNAL, cv2.CHAIN_APPROX_SIMPLE)
    min_area = min_area_fraction * proc.shape[0] * proc.shape[1]
    scored = [(cv2.contourArea(c), c) for c in contours]
    scored = sorted((item for item in scored if item[0] >= min_area and item[0] > 0),
                    key=lambda item: -item[0])[:max_proposals]
    erode = cv2.getStructuringElement(cv2.MORPH_RECT, (3, 3))
    candidates = []
    for area, contour in scored:
        x, y, w, h = cv2.boundingRect(contour)
        mask = np.zeros((h, w), np.uint8)
        cv2.drawContours(mask, [contour], -1, 255, thickness=cv2.FILLED, offset=(-x, -y))
        # Boundary pixels blend object and background; sample the interior.
        inner = cv2.erode(mask, erode)
        sample = inner if cv2.countNonZero(inner) > 0 else mask
        candidate = Candidate()
        candidate.box = [round(x / scale, 2), round(y / scale, 2), round(w / scale, 2), round(h / scale, 2)]
        candidate.area_px = int(round(area / (scale * scale)))
        candidate.palette = palette(lab[y:y + h, x:x + w][sample > 0])
        candidate.silhouette = silhouette(contour)
        candidate.proc_box, candidate.mask, candidate.scale = (x, y, w, h), mask, scale
        candidates.append(candidate)
    return candidates


def measure(candidate, depth, scale_m, intrinsics):
    """Metric size, distance and bearing from a depth frame already at the
    decoded frame's size. None when the mask has no valid depth."""
    frame_h, frame_w = depth.shape[:2]
    px, py, pw, ph = candidate.proc_box
    s = candidate.scale
    x0, y0 = min(frame_w, int(math.floor(px / s))), min(frame_h, int(math.floor(py / s)))
    x1, y1 = min(frame_w, int(math.ceil((px + pw) / s))), min(frame_h, int(math.ceil((py + ph) / s)))
    if x1 <= x0 or y1 <= y0:
        return None
    region = depth[y0:y1, x0:x1]
    mask = candidate.mask
    if mask.shape != region.shape:
        mask = cv2.resize(mask, (x1 - x0, y1 - y0), interpolation=cv2.INTER_NEAREST)
    values = region[(mask > 0) & (region > 0)]
    if values.size == 0:
        return None
    distance = float(np.median(values)) * scale_m
    fx, fy, cx = intrinsics["fx"], intrinsics["fy"], intrinsics["cx"]
    bx, _, bw, bh = candidate.box
    return {"width_m": round(bw * distance / fx, 4), "height_m": round(bh * distance / fy, 4),
            "distance_m": round(distance, 4),
            "bearing_deg": round(math.degrees(math.atan((bx + bw / 2 - cx) / fx)), 3)}


def pair_period_seconds(config):
    """Largest RGB to depth timestamp gap that still counts as one frame."""
    rate = config.get("rate")
    return max(MIN_PAIR_PERIOD_SECONDS, 1.0 / rate) if rate else MIN_PAIR_PERIOD_SECONDS


def paired_depth(config, depth, boot_nanos):
    if config.get("depth") is None or depth is None or boot_nanos is None or depth.boot_nanos is None:
        return None
    if abs(depth.boot_nanos - boot_nanos) > pair_period_seconds(config) * 1e9:
        return None
    return depth.pixels


def score(bgr, config, depth=None, boot_nanos=None):
    """Proposals for one decoded BGR frame. Returns (depth_paired, proposals)."""
    candidates = propose(bgr, config["max_proposals"], config["min_area_fraction"])
    pixels = paired_depth(config, depth, boot_nanos)
    if pixels is None:
        return False, [c.wire() for c in candidates]
    frame_h, frame_w = bgr.shape[:2]
    if pixels.shape != (frame_h, frame_w):
        pixels = cv2.resize(pixels, (frame_w, frame_h), interpolation=cv2.INTER_NEAREST)
    depth_config = config["depth"]
    return True, [c.wire(measure(c, pixels, depth_config["scale_m"], depth_config["intrinsics"]))
                  for c in candidates]


def validate(config):
    if config.get("proposer") not in PROPOSERS:
        raise ValueError("unsupported proposer")
    rate, every = config.get("rate"), config.get("every_frames")
    if (rate is None) == (every is None):
        raise ValueError("exactly one of rate and every_frames is required")
    if rate is not None and not (0 < rate <= 30):
        raise ValueError("rate must be in (0, 30]")
    if every is not None and not (isinstance(every, int) and 1 <= every <= 300):
        raise ValueError("every_frames must be in [1, 300]")
    if not (isinstance(config.get("max_proposals"), int) and config["max_proposals"] >= 1):
        raise ValueError("max_proposals must be positive")
    if not (0 <= config.get("min_area_fraction", -1) < 1):
        raise ValueError("min_area_fraction must be in [0, 1)")
    depth = config.get("depth")
    if depth is not None:
        intrinsics = depth.get("intrinsics") or {}
        if not (depth.get("scale_m", 0) > 0 and intrinsics.get("fx", 0) > 0 and intrinsics.get("fy", 0) > 0
                and "cx" in intrinsics and "cy" in intrinsics):
            raise ValueError("depth needs a positive scale_m and intrinsics fx, fy, cx, cy")
    config.setdefault("pairs", {})
    return config


def run(config):
    lock = threading.Lock()
    decoders = {}
    depths = {}
    pairs = config.get("pairs") or {}
    stopped = threading.Event()

    def new_slot():
        return FrameSlot(rate=config.get("rate"), every_frames=config.get("every_frames"))

    def receive_depth(item, source_id, generation):
        with lock:
            current = depths.get(source_id)
            if item.get("end"):
                if current and current.generation == generation:
                    depths.pop(source_id)
                return
        try:
            if item.get("encoding") != "z16":
                raise ValueError("unsupported depth encoding")
            payload = base64.b64decode(item.get("payload", ""), validate=True)
            pixels = parse_z16(payload, item.get("width"), item.get("height"))
        except ValueError as exc:
            with lock:
                depths.pop(source_id, None)
            emit({"type": "source_error", "source_id": source_id, "generation": generation,
                  "error": "depth: " + str(exc)})
            return
        with lock:
            # Only the latest depth frame per source is kept.
            depths[source_id] = DepthFrame(generation, item.get("boot_nanos"), pixels)

    def receive():
        try:
            while line := sys.stdin.buffer.readline(16 << 20):
                item = json.loads(line)
                source_id, generation = item["source_id"], item["generation"]
                kind = item.get("kind", "rgb")
                if kind == "depth":
                    receive_depth(item, source_id, generation)
                    continue
                with lock:
                    decoder = decoders.get(source_id)
                    # A late close from a retired subscription cannot close its replacement.
                    if item.get("end"):
                        if decoder and decoder.generation == generation:
                            decoders.pop(source_id).stop()
                        continue
                    encoding = item.get("encoding")
                    if kind != "rgb" or encoding not in ("h264", "vp8"):
                        if decoder:
                            decoders.pop(source_id).stop()
                        emit({"type": "source_error", "source_id": source_id, "generation": generation,
                              "error": "unsupported source kind" if kind != "rgb" else "unsupported camera encoding"})
                        continue
                    payload = base64.b64decode(item["payload"], validate=True)
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
                        decoder = Decoder(source_id, generation, encoding, initialization, new_slot())
                        decoders[source_id] = decoder
                    meta = (item.get("sample_id"), item.get("boot_nanos"))
                    if not decoder.stream.feed(payload, meta):
                        # Never splice an encoded stream after losing bytes. The next
                        # sample starts a new decoder and waits for a random-access unit.
                        decoder.stop()
                        emit({"type": "source_error", "source_id": source_id, "generation": generation,
                              "error": "decoder queue overflow; resynchronizing"})
        except Exception as exc:
            emit({"type": "error", "error": "input: " + type(exc).__name__})
        finally:
            stopped.set()

    threading.Thread(target=receive, daemon=True).start()
    try:
        while not stopped.is_set():
            with lock:
                current = list(decoders.values())
            for decoder in current:
                taken = decoder.take(time.monotonic())
                if taken is None:
                    continue
                frame, meta = taken
                sample_id, boot_nanos = meta if meta is not None else (None, None)
                bgr = frame.to_ndarray(format="bgr24")
                with lock:
                    depth = depths.get(pairs.get(decoder.source_id))
                try:
                    depth_paired, proposals = score(bgr, config, depth, boot_nanos)
                except cv2.error as exc:
                    emit({"type": "source_error", "source_id": decoder.source_id,
                          "generation": decoder.generation, "error": "proposer: " + type(exc).__name__})
                    continue
                with lock:
                    # A source reset while scoring ran invalidates the result.
                    if decoders.get(decoder.source_id) is not decoder or decoder.stream.stopped:
                        continue
                emit({"type": "proposals", "source_id": decoder.source_id, "generation": decoder.generation,
                      "sample_id": sample_id, "boot_nanos": boot_nanos,
                      "frame_w": int(bgr.shape[1]), "frame_h": int(bgr.shape[0]),
                      "achieved_fps": round(decoder.meter.tick(time.monotonic()), 3),
                      "depth_paired": depth_paired, "proposals": proposals})
            stopped.wait(0.02)
    finally:
        with lock:
            for decoder in decoders.values():
                decoder.stop()


def main():
    try:
        config = validate(json.loads(sys.stdin.buffer.readline()))
    except (ValueError, TypeError, AttributeError) as exc:
        print("invalid world view config: " + str(exc), file=sys.stderr, flush=True)
        sys.exit(2)
    cv2.setNumThreads(2)
    emit({"type": "ready"})
    run(config)


if __name__ == "__main__":
    main()
