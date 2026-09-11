"""Lossless, bounded whole-file batches for complete pull-request reviews."""

from __future__ import annotations

import re


class DiffBatchError(ValueError):
    """A complete diff cannot be represented by bounded whole-file batches."""


def split_diff(diff: str, max_bytes: int) -> list[str]:
    """Keep every input byte in order, never splitting an individual file patch.

    Callers must validate the complete diff against revision metadata before
    reviewing the batches. This helper only enforces lossless byte budgeting.
    An oversized file is rejected before any partial review can begin.
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
            raise DiffBatchError(
                f"An individual file patch exceeds the {max_bytes:,}-byte review batch limit; "
                "split that file change. No partial review was performed"
            )
        if current and current_bytes + patch_bytes > max_bytes:
            batches.append("".join(current))
            current = []
            current_bytes = 0
        current.append(patch)
        current_bytes += patch_bytes
    batches.append("".join(current))
    return batches
