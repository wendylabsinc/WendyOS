"""Latest-only asynchronous vision with capture-time freshness enforcement."""
from __future__ import annotations

from dataclasses import dataclass
import threading
import time
from typing import Any, Callable


class VisionUnavailable(RuntimeError):
    """No admissible cached embedding exists for this control tick."""


@dataclass(frozen=True)
class VisionFrame:
    frame_id: int
    stream_id: str
    captured_at_ns: int
    payload: Any


@dataclass(frozen=True)
class VisionSnapshot:
    episode_generation: int
    generation: int
    frame_id: int
    stream_id: str
    captured_at_ns: int
    completed_at_ns: int
    elapsed_ms: float
    embedding: Any


class AsyncVisionBuffer:
    """Encode only the newest pending frame and atomically publish snapshots.

    The worker and controller never wait on one another. Two published slots
    preserve the latest completed value while its replacement is prepared.
    Capture time—not encode completion time—determines freshness.
    """

    def __init__(
        self,
        encode: Callable[[Any], Any],
        *,
        maximum_capture_age_s: float,
        clock_ns: Callable[[], int] = time.time_ns,
    ) -> None:
        if not 0 < maximum_capture_age_s <= 1:
            raise ValueError("maximum_capture_age_s must be within (0, 1]")
        self._encode = encode
        self._maximum_age_ns = int(maximum_capture_age_s * 1_000_000_000)
        self._clock_ns = clock_ns
        self._condition = threading.Condition()
        self._stop = False
        self._pending: tuple[int, VisionFrame] | None = None
        self._slots: list[VisionSnapshot | None] = [None, None]
        self._published_slot = 0
        self._generation = 0
        self._episode_generation = 0
        self._last_submitted: tuple[str, int] | None = None
        self._thread: threading.Thread | None = None
        self._submitted = 0
        self._processed = 0
        self._overwritten = 0
        self._duplicate_or_old = 0
        self._errors = 0
        self._stale_reads = 0
        self._discarded_on_reset = 0
        self._last_error: str | None = None

    def start(self) -> None:
        with self._condition:
            if self._thread is not None:
                raise RuntimeError("vision worker already started")
            self._thread = threading.Thread(target=self._run, name="policy-vision", daemon=True)
            self._thread.start()

    def submit(self, frame: VisionFrame) -> bool:
        if frame.frame_id < 0 or not frame.stream_id or frame.captured_at_ns <= 0:
            raise ValueError("invalid frame identity or capture time")
        with self._condition:
            if self._stop:
                raise RuntimeError("vision worker is closed")
            identity = (frame.stream_id, frame.frame_id)
            if self._last_submitted is not None:
                old_stream, old_frame = self._last_submitted
                if frame.stream_id == old_stream and frame.frame_id <= old_frame:
                    self._duplicate_or_old += 1
                    return False
            if self._pending is not None:
                self._overwritten += 1
            self._pending = (self._episode_generation, frame)
            self._last_submitted = identity
            self._submitted += 1
            self._condition.notify()
            return True

    def reset_episode(self) -> int:
        """Invalidate every pre-reset frame, including one being encoded.

        The monotonically increasing episode generation is captured before an
        encode begins and checked again before publication.  This prevents a
        slow frame from the preceding episode from becoming the first image of
        a newly zeroed recurrent episode.
        """
        with self._condition:
            if self._stop:
                raise RuntimeError("vision worker is closed")
            self._episode_generation += 1
            if self._pending is not None:
                self._discarded_on_reset += 1
            self._pending = None
            self._slots = [None, None]
            self._published_slot = 0
            self._last_submitted = None
            self._condition.notify_all()
            return self._episode_generation

    def latest(self, *, control_at_ns: int | None = None) -> VisionSnapshot:
        now = self._clock_ns() if control_at_ns is None else control_at_ns
        with self._condition:
            snapshot = self._slots[self._published_slot]
            if snapshot is None:
                raise VisionUnavailable("no completed vision embedding")
            return self._admit_locked(snapshot, now)

    def admit(self, snapshot: VisionSnapshot, *, control_at_ns: int) -> VisionSnapshot:
        """Revalidate a previously selected immutable snapshot for this tick."""
        with self._condition:
            return self._admit_locked(snapshot, control_at_ns)

    def _admit_locked(self, snapshot: VisionSnapshot, now: int) -> VisionSnapshot:
        if snapshot.episode_generation != self._episode_generation:
            self._stale_reads += 1
            raise VisionUnavailable("vision embedding belongs to a previous episode")
        age_ns = now - snapshot.captured_at_ns
        if age_ns < 0:
            self._stale_reads += 1
            raise VisionUnavailable("camera timestamp is in the future")
        if age_ns > self._maximum_age_ns:
            self._stale_reads += 1
            raise VisionUnavailable(f"vision embedding is stale ({age_ns / 1e6:.1f} ms)")
        return snapshot

    def status(self) -> dict[str, Any]:
        with self._condition:
            snapshot = self._slots[self._published_slot]
            return {
                "submitted": self._submitted,
                "processed": self._processed,
                "pending_overwrites": self._overwritten,
                "duplicate_or_old_frames": self._duplicate_or_old,
                "encode_errors": self._errors,
                "stale_reads": self._stale_reads,
                "discarded_on_episode_reset": self._discarded_on_reset,
                "last_error": self._last_error,
                "episode_generation": self._episode_generation,
                "latest_generation": snapshot.generation if snapshot else None,
                "latest_frame_id": snapshot.frame_id if snapshot else None,
                "latest_stream_id": snapshot.stream_id if snapshot else None,
                "latest_capture_age_ms": (
                    max(0.0, (self._clock_ns() - snapshot.captured_at_ns) / 1e6)
                    if snapshot
                    else None
                ),
                "latest_encode_ms": snapshot.elapsed_ms if snapshot else None,
            }

    def close(self, timeout_s: float = 2.0) -> None:
        with self._condition:
            self._stop = True
            self._condition.notify_all()
            thread = self._thread
        if thread is not None:
            thread.join(timeout=timeout_s)
            if thread.is_alive():
                raise RuntimeError("vision worker did not stop")

    def _run(self) -> None:
        while True:
            with self._condition:
                while self._pending is None and not self._stop:
                    self._condition.wait()
                if self._stop:
                    return
                episode_generation, frame = self._pending
                self._pending = None
            assert frame is not None
            started = time.perf_counter_ns()
            try:
                embedding = self._encode(frame.payload)
                completed = self._clock_ns()
                elapsed_ms = (time.perf_counter_ns() - started) / 1e6
                with self._condition:
                    if episode_generation != self._episode_generation:
                        self._discarded_on_reset += 1
                        continue
                    self._generation += 1
                    next_slot = 1 - self._published_slot
                    self._slots[next_slot] = VisionSnapshot(
                        episode_generation=episode_generation,
                        generation=self._generation,
                        frame_id=frame.frame_id,
                        stream_id=frame.stream_id,
                        captured_at_ns=frame.captured_at_ns,
                        completed_at_ns=completed,
                        elapsed_ms=elapsed_ms,
                        embedding=embedding,
                    )
                    self._published_slot = next_slot
                    self._processed += 1
                    self._last_error = None
                    self._condition.notify_all()
            except Exception as exc:  # fail closed while preserving last good data
                with self._condition:
                    self._errors += 1
                    self._last_error = f"{type(exc).__name__}: {exc}"
