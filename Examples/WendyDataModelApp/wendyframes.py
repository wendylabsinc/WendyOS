"""Read agent-fed loopback frames and their exact hub sample IDs.

The copied V4L2 timestamp carries sample_id = tv_sec*1e6 + tv_usec, not a
clock reading. The kernel sequence is independent and detects reader drops.
The app records its own dequeue time separately from the sample identity.
"""

from __future__ import annotations

import ctypes
from collections import deque
from fractions import Fraction
import fcntl
import logging
import mmap
import os
import select
import time
from dataclasses import dataclass

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


PIX_H264 = _fourcc("H264")
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


class _BufferMemory(ctypes.Union):
    # videodev2.h's m union includes pointers, even for MMAP's uint32 offset.
    _fields_ = [("offset", ctypes.c_uint32), ("userptr", ctypes.c_ulong),
                ("planes", ctypes.c_void_p), ("fd", ctypes.c_int32)]


class _Buffer(ctypes.Structure):
    """v4l2_buffer. 88 bytes: timestamp at 24, sequence at 56."""

    _anonymous_ = ("m",)
    _fields_ = [
        ("index", ctypes.c_uint32), ("type", ctypes.c_uint32),
        ("bytesused", ctypes.c_uint32), ("flags", ctypes.c_uint32),
        ("field", ctypes.c_uint32), ("timestamp", _Timeval),
        ("timecode", _Timecode), ("sequence", ctypes.c_uint32),
        ("memory", ctypes.c_uint32), ("m", _BufferMemory),
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
        # v4l2_window contains pointers, aligning the union to8 on Linux64.
        _fields_ = [("pix", _PixFormat), ("raw", ctypes.c_uint8 * 200),
                    ("_pointer_alignment", ctypes.c_void_p)]

    _fields_ = [("type", ctypes.c_uint32), ("fmt", _U)]


@dataclass
class Frame:
    """One frame plus the identity the episode recorded it under."""

    image: np.ndarray
    source_id: str
    boottime_nanos: int  # App dequeue time, not the agent capture time.
    sample_id: int
    dropped_before: int = 0

    def input_refs(self):
        """Reference the same source and sample ID as the episode ledger."""
        return [{"source_id": self.source_id, "sample_id": self.sample_id}]



class H264Frames:
    """Decode whole access units while retaining each picture's input identity.

    Decoder output order can differ from dequeue order. Private packet PTS values
    carry bounded metadata through the decoder; neither output position nor
    nanosecond conversion is used to reconstruct a hub sample ID.
    """

    def __init__(self, source_id):
        import av
        self._av = av
        self._codec = av.CodecContext.create("h264", "r")
        self._codec.thread_count = 1
        self._source_id = source_id
        self._next = 0
        self._pending = {}

    def decode(self, payload, sample_id, boottime, dropped):
        if len(self._pending) >= 128:
            raise RuntimeError("H264 decoder retained too many unreturned sample identities")
        self._next += 1
        token = self._next
        self._pending[token] = (sample_id, boottime, dropped)
        packet = self._av.Packet(payload)
        packet.pts = token
        packet.time_base = Fraction(1, 1_000_000)
        frames = []
        try:
            pictures = self._codec.decode(packet)
        except self._av.FFmpegError as error:
            self._pending.clear()
            raise RuntimeError("H264 access unit could not be decoded with a trustworthy identity") from error
        for picture in pictures:
            metadata = self._pending.pop(picture.pts, None)
            if metadata is None:
                raise RuntimeError("H264 decoder returned a picture without its input identity")
            identity, receipt, lost = metadata
            frames.append(Frame(image=picture.to_ndarray(format="bgr24"), source_id=self._source_id,
                                boottime_nanos=receipt, sample_id=identity, dropped_before=lost))
        return frames


class CameraNode:
    """A v4l2loopback node fed by the agent."""

    def __init__(self, path: str, source_id: str = "", buffers: int = 4):
        self.path = path
        self.source_id = source_id or path
        self._fd = os.open(path, os.O_RDWR)
        self._maps: list[mmap.mmap] = []
        self._last_sequence: int | None = None
        self._decoded = deque()
        self._decoder = None
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
        if self.pixelformat not in (PIX_YUYV, PIX_MJPEG, PIX_BGR24, PIX_RGB24, PIX_H264):
            raise RuntimeError(
                f"{self.path} offers an unsupported pixel format {self.pixelformat:#x}; "
                "expected H264, YUYV, MJPEG, BGR3 or RGB3"
            )
        if self.pixelformat == PIX_H264:
            self._decoder = H264Frames(self.source_id)
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
        if getattr(self, "_decoder", None) is not None:
            while not self._decoded:
                self._read_buffer()
            return self._decoded.popleft()
        return self._read_buffer()

    def _read_buffer(self):
        buf = _Buffer()
        buf.type, buf.memory = V4L2_BUF_TYPE_VIDEO_CAPTURE, V4L2_MEMORY_MMAP
        fcntl.ioctl(self._fd, VIDIOC_DQBUF, buf)
        try:
            payload = self._maps[buf.index][: buf.bytesused]
            if buf.flags & 0xE000 != 0x4000:
                raise RuntimeError("camera node did not preserve the agent sample ID")
            sample_id = buf.timestamp.tv_sec * 1_000_000 + buf.timestamp.tv_usec
            if not 0 <= buf.timestamp.tv_usec < 1_000_000 or not 0 < sample_id < 1 << 64:
                raise RuntimeError("camera node returned an invalid sample ID")
            boottime = time.clock_gettime_ns(time.CLOCK_BOOTTIME)
            dropped = 0
            if self._last_sequence is not None:
                gap = buf.sequence - self._last_sequence - 1
                dropped = gap if gap > 0 else 0
            self._last_sequence = buf.sequence
            if getattr(self, "_decoder", None) is not None:
                self._decoded.extend(self._decoder.decode(payload, sample_id, boottime, dropped))
                return None
            return Frame(image=self._decode(payload), source_id=self.source_id,
                         boottime_nanos=boottime, sample_id=sample_id, dropped_before=dropped)
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
        return bool(getattr(self, "_decoded", None)) or bool(select.select([self._fd], [], [], 0)[0])

    def frames(self):
        while True:
            yield self.read()

    def close(self) -> None:
        try:
            arg = ctypes.c_uint32(V4L2_BUF_TYPE_VIDEO_CAPTURE)
            fcntl.ioctl(self._fd, VIDIOC_STREAMOFF, arg)
        except OSError:
            log.debug("stream-off failed during camera cleanup", exc_info=True)
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
