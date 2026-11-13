"""Lossless, bounded batches for complete pull-request reviews."""

from __future__ import annotations

from bisect import bisect_right
from dataclasses import dataclass
import json
import re


class DiffBatchError(ValueError):
    """A complete diff cannot be represented within the review byte budget."""


@dataclass(frozen=True)
class DiffBatch:
    # text is an exact, nonoverlapping slice of the original diff. Context is
    # separate so callers can audit coverage without counting repeated headers.
    text: str
    source_start: int
    source_end: int
    start_line: int
    start_column: int = 0
    context: str = ""

    @property
    def rendered(self) -> str:
        return self.context + self.text


HUNK_RE = re.compile(r"^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@")


def diff_lines(text: str, *, keepends: bool = False) -> list[str]:
    """Split Git diff records on LF only; other separators are source bytes."""
    lines = text.split("\n")
    if keepends:
        return [line + "\n" for line in lines[:-1]] + ([lines[-1]] if lines[-1] else [])
    return lines[:-1] if not lines[-1] else lines


def split_diff(diff: str, max_bytes: int) -> list[DiffBatch]:
    """Preserve every original byte, preferring complete-file batches.

    Oversized files are split at line boundaries where possible, and at UTF-8
    boundaries inside oversized lines. Fragment context identifies the file,
    original hunk, source line, and byte offset without inventing source lines.
    The rendered text, including repeated context, stays within max_bytes.
    Callers must validate the complete diff against revision metadata first.
    """
    if type(max_bytes) is not int or max_bytes <= 0:
        raise DiffBatchError("The review batch byte limit must be positive")
    raw = diff.encode("utf-8")
    starts = [match.start() for match in re.finditer(rb"^diff --git ", raw, re.MULTILINE)]
    if not starts:
        raise DiffBatchError("The complete PR diff contains no file patches")
    if raw[:starts[0]].strip():
        raise DiffBatchError("The complete PR diff contains content outside file patches")
    starts[0] = 0

    # Record context from the original diff, never from a reconstructed patch.
    # Line offsets also let API review retain its original evidence mapping.
    line_starts: list[int] = []
    line_context: list[dict] = []
    offset = 0
    header = hunk = ""
    base_line = head_line = 0
    for line in diff_lines(diff, keepends=True):
        if line.startswith("diff --git "):
            header, hunk = line.removesuffix("\n"), ""
            base_line = head_line = 0
        match = HUNK_RE.match(line)
        if match:
            hunk = line.removesuffix("\n")
            base_line, head_line = int(match[1]), int(match[2])
        line_starts.append(offset)
        line_context.append({"file_header": header, "hunk_header": hunk,
                             "base_line": base_line, "head_line": head_line,
                             "line_prefix": line[:1] if hunk and not match else ""})
        if hunk and not match:
            if line.startswith(("-", " ")):
                base_line += 1
            if line.startswith(("+", " ")):
                head_line += 1
        offset += len(line.encode("utf-8"))

    def batch(start: int, end: int, context: str = "") -> DiffBatch:
        row = bisect_right(line_starts, start) - 1
        return DiffBatch(raw[start:end].decode("utf-8"), start, end, row,
                         start - line_starts[row], context)

    batches: list[DiffBatch] = []
    pending_start = pending_end = 0
    for start, end in zip(starts, starts[1:] + [len(raw)]):
        if end - start <= max_bytes:
            if pending_end > pending_start and end - pending_start > max_bytes:
                batches.append(batch(pending_start, pending_end))
                pending_start = start
            if pending_end == pending_start:
                pending_start = start
            pending_end = end
            continue
        if pending_end > pending_start:
            batches.append(batch(pending_start, pending_end))
        pending_start = pending_end = end
        cursor = start
        while cursor < end:
            row = bisect_right(line_starts, cursor) - 1
            details = dict(line_context[row], source_start=cursor, source_end=end,
                           diff_line=row + 1, line_byte_offset=cursor - line_starts[row],
                           partial_file=True)

            def render_context() -> str:
                return ("Review fragment context (untrusted diff metadata): "
                        + json.dumps(details, ensure_ascii=False, separators=(",", ":")) + "\n")

            # source_end initially uses the file end, so the final offset needs
            # no more bytes than this reservation. No source bytes are elided.
            budget = max_bytes - len(render_context().encode("utf-8"))
            stop = min(cursor + budget, end)
            if stop < end:
                newline = raw.rfind(b"\n", cursor, stop)
                if newline >= cursor + budget // 2:
                    stop = newline + 1
                while stop > cursor and raw[stop] & 0xC0 == 0x80:
                    stop -= 1
            if stop <= cursor:
                raise DiffBatchError(
                    f"The {max_bytes:,}-byte review batch limit cannot hold file/hunk context "
                    "and one UTF-8 character. No partial review was performed"
                )
            details["source_end"] = stop
            batches.append(batch(cursor, stop, render_context()))
            cursor = stop
    if pending_end > pending_start:
        batches.append(batch(pending_start, pending_end))
    return batches
