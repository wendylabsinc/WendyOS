"""Read frames from the agent-fed camera node, with their identity.

The agent owns the physical camera and feeds a v4l2loopback node that this app
opens. Two things follow from that, and they are the whole reason this module
exists instead of a bare cv2.VideoCapture:

  * The app is not an independent second reader. The frame it scores is the
    frame the episode recorded, so a prediction can name its input and the join
    resolves.
  * Identity rides in-band. The agent writes the frame's canonical
    CLOCK_BOOTTIME receipt into the V4L2 buffer timestamp, which v4l2loopback
    preserves. cv2 does not expose that field, so this module drives V4L2
    directly and hands the timestamp back with the pixels.

The buffer's `sequence` is NOT identity: v4l2loopback overwrites it with its own
counter on QBUF. Gaps are still meaningful as a dropped-frame signal, which is
what dropped_before reports, but the boottime is what names the sample.
"""

from __future__ import annotations

import ctypes
import fcntl
import logging
import mmap
import os
import select
import time
from dataclasses import dataclass, field

import cv2
import numpy as np

log = logging.getLogger("wendyframes")

V4L2_BUF_TYPE_VIDEO_CAPTURE = 1
V4L2_MEMORY_MMAP = 1
V4L2_FIELD_NONE = 1


def _iowr(nr: int, size: int) -> int:
    return (3 << 30) | (size << 16) | (ord("V") << 8) | nr


def _iow(nr: int, size: int) -> int:
    return (1 << 30) | (size << 16) | (ord("V") << 8) | nr


VIDIOC_G_FMT = _iowr(4, 208)
VIDIOC_S_FMT = _iowr(5, 208)
VIDIOC_REQBUFS = _iowr(8, 20)
VIDIOC_QUERYBUF = _iowr(9, 88)
VIDIOC_QBUF = _iowr(15, 88)
VIDIOC_DQBUF = _iowr(17, 88)
VIDIOC_STREAMON = _iow(18, 4)
VIDIOC_STREAMOFF = _iow(19, 4)


def _fourcc(code: str) -> int:
    return (ord(code[0]) | ord(code[1]) << 8 | ord(code[2]) << 16 | ord(code[3]) << 24)


PIX_YUYV = _fourcc("YUYV")
PIX_MJPEG = _fourcc("MJPG")
PIX_BGR24 = _fourcc("BGR3")
PIX_RGB24 = _fourcc("RGB3")


class _Timeval(ctypes.Structure):
    _fields_ = [("tv_sec", ctypes.c_long), ("tv_usec", ctypes.c_long)]


class _Timecode(ctypes.Structure):
    _fields_ = [
        ("type", ctypes.c_uint32), ("flags", ctypes.c_uint32),
        ("frames", ctypes.c_uint8), ("seconds", ctypes.c_uint8),
        ("minutes", ctypes.c_uint8), ("hours", ctypes.c_uint8),
        ("userbits", ctypes.c_uint8 * 4),
    ]


class _Buffer(ctypes.Structure):
    """v4l2_buffer. 88 bytes: timestamp at 24, sequence at 56."""

    _fields_ = [
        ("index", ctypes.c_uint32), ("type", ctypes.c_uint32),
        ("bytesused", ctypes.c_uint32), ("flags", ctypes.c_uint32),
        ("field", ctypes.c_uint32), ("timestamp", _Timeval),
        ("timecode", _Timecode), ("sequence", ctypes.c_uint32),
        ("memory", ctypes.c_uint32), ("offset", ctypes.c_uint32),
        ("length", ctypes.c_uint32), ("reserved2", ctypes.c_uint32),
        ("request_fd", ctypes.c_int32),
    ]


class _RequestBuffers(ctypes.Structure):
    _fields_ = [
        ("count", ctypes.c_uint32), ("type", ctypes.c_uint32),
        ("memory", ctypes.c_uint32), ("capability", ctypes.c_uint32),
        ("flags", ctypes.c_uint8), ("reserved", ctypes.c_uint8 * 3),
    ]


class _PixFormat(ctypes.Structure):
    _fields_ = [
        ("width", ctypes.c_uint32), ("height", ctypes.c_uint32),
        ("pixelformat", ctypes.c_uint32), ("field", ctypes.c_uint32),
        ("bytesperline", ctypes.c_uint32), ("sizeimage", ctypes.c_uint32),
        ("colorspace", ctypes.c_uint32), ("priv", ctypes.c_uint32),
        ("flags", ctypes.c_uint32), ("ycbcr_enc", ctypes.c_uint32),
        ("quantization", ctypes.c_uint32), ("xfer_func", ctypes.c_uint32),
    ]


class _Format(ctypes.Structure):
    class _U(ctypes.Union):
        _fields_ = [("pix", _PixFormat), ("raw", ctypes.c_uint8 * 200)]

    _fields_ = [("type", ctypes.c_uint32), ("fmt", _U)]


@dataclass
class Frame:
    """One frame plus the identity the episode recorded it under."""

    image: np.ndarray
    source_id: str
    boottime_nanos: int
    dropped_before: int = 0
    sample_ids: list = field(default_factory=list)

    def input_refs(self):
        """The provenance join: what this prediction was computed from.

        The agent names the sample by its canonical boottime, so that is the
        reference. It is microsecond-truncated by struct timeval, which is
        exact rather than lossy: 1e9 divides by 1000, so the value here is
        always the recorded boottime with its sub-microsecond digits dropped.
        """
        return [{"source_id": self.source_id, "boottime_nanos": self.boottime_nanos}]


class CameraNode:
    """A v4l2loopback node fed by the agent."""

    def __init__(self, path: str, source_id: str = "", buffers: int = 4):
        self.path = path
        self.source_id = source_id or path
        self._fd = os.open(path, os.O_RDWR)
        self._maps: list[mmap.mmap] = []
        self._last_sequence: int | None = None
        try:
            self._configure()
            self._request_buffers(buffers)
            self._start()
        except Exception:
            self.close()
            raise

    def _configure(self) -> None:
        fmt = _Format()
        fmt.type = V4L2_BUF_TYPE_VIDEO_CAPTURE
        fcntl.ioctl(self._fd, VIDIOC_G_FMT, fmt)
        self.width = fmt.fmt.pix.width
        self.height = fmt.fmt.pix.height
        self.pixelformat = fmt.fmt.pix.pixelformat
        if self.pixelformat not in (PIX_YUYV, PIX_MJPEG, PIX_BGR24, PIX_RGB24):
            raise RuntimeError(
                f"{self.path} offers an unsupported pixel format {self.pixelformat:#x}; "
                "the agent feeds YUYV, MJPEG, BGR3 or RGB3"
            )
        log.info("%s: %dx%d", self.path, self.width, self.height)

    def _request_buffers(self, count: int) -> None:
        req = _RequestBuffers()
        req.count, req.type, req.memory = count, V4L2_BUF_TYPE_VIDEO_CAPTURE, V4L2_MEMORY_MMAP
        fcntl.ioctl(self._fd, VIDIOC_REQBUFS, req)
        if req.count < 1:
            raise RuntimeError(f"{self.path} granted no buffers")
        for i in range(req.count):
            buf = _Buffer()
            buf.index, buf.type, buf.memory = i, V4L2_BUF_TYPE_VIDEO_CAPTURE, V4L2_MEMORY_MMAP
            fcntl.ioctl(self._fd, VIDIOC_QUERYBUF, buf)
            self._maps.append(
                mmap.mmap(self._fd, buf.length, mmap.MAP_SHARED,
                          mmap.PROT_READ | mmap.PROT_WRITE, offset=buf.offset)
            )
            fcntl.ioctl(self._fd, VIDIOC_QBUF, buf)

    def _start(self) -> None:
        arg = ctypes.c_uint32(V4L2_BUF_TYPE_VIDEO_CAPTURE)
        fcntl.ioctl(self._fd, VIDIOC_STREAMON, arg)

    def read(self) -> Frame:
        """Dequeue the next frame. Blocks until one is available."""
        buf = _Buffer()
        buf.type, buf.memory = V4L2_BUF_TYPE_VIDEO_CAPTURE, V4L2_MEMORY_MMAP
        fcntl.ioctl(self._fd, VIDIOC_DQBUF, buf)
        try:
            payload = self._maps[buf.index][: buf.bytesused]
            image = self._decode(payload)
            boottime = buf.timestamp.tv_sec * 1_000_000_000 + buf.timestamp.tv_usec * 1_000
            dropped = 0
            if self._last_sequence is not None:
                gap = buf.sequence - self._last_sequence - 1
                dropped = gap if gap > 0 else 0
            self._last_sequence = buf.sequence
            return Frame(image=image, source_id=self.source_id,
                         boottime_nanos=boottime, dropped_before=dropped)
        finally:
            fcntl.ioctl(self._fd, VIDIOC_QBUF, buf)

    def _decode(self, payload: bytes) -> np.ndarray:
        if self.pixelformat == PIX_MJPEG:
            return cv2.imdecode(np.frombuffer(payload, np.uint8), cv2.IMREAD_COLOR)
        if self.pixelformat == PIX_YUYV:
            yuyv = np.frombuffer(payload, np.uint8).reshape(self.height, self.width, 2)
            return cv2.cvtColor(yuyv, cv2.COLOR_YUV2BGR_YUYV)
        arr = np.frombuffer(payload, np.uint8).reshape(self.height, self.width, 3)
        return arr if self.pixelformat == PIX_BGR24 else cv2.cvtColor(arr, cv2.COLOR_RGB2BGR)

    def readable(self) -> bool:
        """Whether another frame is already waiting, without blocking."""
        return bool(select.select([self._fd], [], [], 0)[0])

    def frames(self):
        while True:
            yield self.read()

    def close(self) -> None:
        try:
            arg = ctypes.c_uint32(V4L2_BUF_TYPE_VIDEO_CAPTURE)
            fcntl.ioctl(self._fd, VIDIOC_STREAMOFF, arg)
        except OSError:
            pass
        for m in self._maps:
            m.close()
        self._maps.clear()
        os.close(self._fd)

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        self.close()


def freshest_frames(node: CameraNode):
    """Yield (frame, discarded), skipping whatever queued while you were busy.

    Inference is slower than capture, so a plain loop falls behind the producer
    and the identifiers a prediction references drift out of the range the
    episode recorded, which is what makes the join unresolvable. This drains the
    driver queue to its newest frame and reports how many it dropped, so the
    caller can account for every frame that arrived and was not scored.
    """
    while True:
        frame = node.read()
        discarded = 0
        # Anything still queued is staler than what we just took. Poll with a
        # zero timeout so this never blocks: an empty queue is the common case.
        while node.readable():
            frame = node.read()
            discarded += 1
        yield frame, discarded


def resilient_frames(path: str, source_id: str, attempts: int, delay: float):
    """freshest_frames, but surviving the node going away and coming back."""
    left = attempts
    node = None
    try:
        while True:
            if node is None:
                try:
                    node = CameraNode(path, source_id=source_id)
                except OSError as exc:
                    if left <= 0:
                        raise
                    left -= 1
                    log.warning("camera node %s unavailable (%s); retrying in %.1fs (%d attempt(s) left)",
                                path, exc, delay, left)
                    time.sleep(delay)
                    continue
            try:
                for frame, discarded in freshest_frames(node):
                    left = attempts  # a delivered frame refills the budget
                    yield frame, discarded
            except OSError as exc:
                log.warning("camera node %s ended (%s); reopening", path, exc)
                node.close()
                node = None
    finally:
        if node is not None:
            node.close()
