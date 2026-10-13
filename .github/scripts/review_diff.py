"""Bounded review batches, preserving all source text and original locations."""

from __future__ import annotations

import re


class DiffBatchError(ValueError):
    """A complete diff cannot be represented within the review byte budget."""


HUNK_RE = re.compile(r"^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@(.*)\n?$")


def fragment_patch(patch: str, max_bytes: int) -> list[str]:
    """Represent an oversized patch as complete, labeled source fragments.

    File metadata is repeated. Small hunks describe each original source line;
    a minified line may occupy several hunks with the SAME source line number.
    Byte ranges refer to its UTF-8 content, excluding the diff prefix/newline.
    These are review excerpts, not patches to apply. No source text is omitted.
    The caller must validate the original complete diff before reviewing them.
    """
    lines = patch.splitlines(keepends=True)
    first = next((i for i, line in enumerate(lines) if HUNK_RE.fullmatch(line)), None)
    if first is None:
        raise DiffBatchError("Oversized metadata-only or binary patch cannot be fragmented")
    header = "".join(lines[:first])
    header_bytes = len(header.encode("utf-8"))
    records = []
    base = head = remaining_base = remaining_head = 0
    for index, line in enumerate(lines[first:], start=first + 1):
        match = HUNK_RE.fullmatch(line)
        if match:
            if remaining_base or remaining_head:
                raise DiffBatchError("Cannot fragment an incomplete diff hunk")
            base, head = int(match[1]), int(match[3])
            remaining_base = int(match[2]) if match[2] is not None else 1
            remaining_head = int(match[4]) if match[4] is not None else 1
            continue
        if line.rstrip("\n") == "\\ No newline at end of file":
            if not records:
                raise DiffBatchError("Unexpected no-newline marker")
            records[-1]["marker"] = line
            continue
        prefix = line[:1]
        consumed_base = int(prefix in ("-", " "))
        consumed_head = int(prefix in ("+", " "))
        if (prefix not in ("-", "+", " ") or consumed_base > remaining_base
                or consumed_head > remaining_head):
            raise DiffBatchError("Cannot fragment an incomplete or unsupported diff record")
        records.append({"base": base, "head": head, "base_count": consumed_base,
                        "head_count": consumed_head, "prefix": prefix,
                        "content": line[1:].removesuffix("\n"), "line": index, "marker": ""})
        base += consumed_base
        head += consumed_head
        remaining_base -= consumed_base
        remaining_head -= consumed_head
    if remaining_base or remaining_head:
        raise DiffBatchError("Cannot fragment an incomplete diff hunk")

    batches = []
    current = header
    current_bytes = header_bytes
    for record in records:
        content = record["content"].encode("utf-8")
        total = len(content)

        def hunk(start, end):
            return (f"@@ -{record['base']},{record['base_count']} "
                    f"+{record['head']},{record['head_count']} @@ "
                    f"review fragment: UTF-8 bytes {start}:{end}/{total} "
                    f"of original diff line {record['line']}\n")

        # Reserve the largest range label and the optional no-newline marker.
        overhead = len((hunk(total, total) + record["prefix"] + "\n" + record["marker"]).encode())
        capacity = max_bytes - header_bytes - overhead
        if capacity < 4:
            raise DiffBatchError("Review batch limit cannot fit file metadata and a source fragment. No partial review was performed")
        start = 0
        while True:
            end = min(total, start + capacity)
            # Do not split a UTF-8 code point across model requests.
            while end < total and content[end] & 0xC0 == 0x80:
                end -= 1
            body = hunk(start, end) + record["prefix"] + content[start:end].decode("utf-8") + "\n"
            if end == total:
                body += record["marker"]
            size = len(body.encode("utf-8"))
            if current_bytes + size > max_bytes:
                batches.append(current)
                current, current_bytes = header, header_bytes
            current += body
            current_bytes += size
            if end == total:
                break
            start = end
    if current != header:
        batches.append(current)
    return batches


def split_diff(diff: str, max_bytes: int) -> list[str]:
    """Keep ordinary file patches unchanged; fragment oversized files explicitly.

    Callers must validate the complete diff against revision metadata before
    reviewing the batches. Oversized files retain every source byte, with file
    metadata, original source line numbers and continuation byte ranges in each
    batch. A failed fragment plan rejects the whole review before model calls.
    """
    if type(max_bytes) is not int or max_bytes <= 0:
        raise DiffBatchError("The review batch byte limit must be positive")
    starts = [match.start() for match in re.finditer(r"^diff --git ", diff, re.MULTILINE)]
    if not starts:
        raise DiffBatchError("The complete PR diff contains no file patches")
    if diff[:starts[0]].strip():
        raise DiffBatchError("The complete PR diff contains content outside file patches")
    # Preserve any harmless leading whitespace as part of the first patch.
    starts[0] = 0
    ends = starts[1:] + [len(diff)]
    batches: list[str] = []
    current: list[str] = []
    current_bytes = 0
    for start, end in zip(starts, ends):
        patch = diff[start:end]
        patch_bytes = len(patch.encode("utf-8"))
        if patch_bytes > max_bytes:
            if current:
                batches.append("".join(current))
                current, current_bytes = [], 0
            batches.extend(fragment_patch(patch, max_bytes))
            continue
        if current and current_bytes + patch_bytes > max_bytes:
            batches.append("".join(current))
            current = []
            current_bytes = 0
        current.append(patch)
        current_bytes += patch_bytes
    if current:
        batches.append("".join(current))
    return batches
