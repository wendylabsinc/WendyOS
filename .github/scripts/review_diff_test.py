#!/usr/bin/env python3
"""Tests for complete review batching; no model or network required."""

from __future__ import annotations

import pathlib
import sys
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).parent))
from review_diff import DiffBatchError, split_diff


def patch(path: str, content: str = "new") -> str:
    return f"diff --git a/{path} b/{path}\n--- a/{path}\n+++ b/{path}\n@@ -1 +1 @@\n-old\n+{content}\n"


class SplitDiffTests(unittest.TestCase):
    def test_small_diff_is_unchanged(self):
        raw = patch("one.go") + patch("two.go")
        self.assertEqual(split_diff(raw, len(raw.encode())), [raw])

    def test_batches_keep_entire_files_and_all_bytes_in_order(self):
        patches = [patch(f"file{i}.go") for i in range(5)]
        limit = len((patches[0] + patches[1]).encode())
        batches = split_diff("".join(patches), limit)
        self.assertEqual(batches, ["".join(patches[:2]), "".join(patches[2:4]), patches[4]])
        self.assertEqual("".join(batches), "".join(patches))
        self.assertTrue(all(len(batch.encode()) <= limit for batch in batches))

    def test_unicode_counts_bytes_not_characters(self):
        patches = [patch("one.go", "é" * 10), patch("two.go", "é" * 10)]
        raw = "".join(patches)
        self.assertEqual(split_diff(raw, len(raw)), patches)

    def test_exact_limit_is_accepted(self):
        raw = patch("one.go", "é")
        self.assertEqual(split_diff(raw, len(raw.encode())), [raw])
        with self.assertRaisesRegex(DiffBatchError, "No partial review"):
            split_diff(raw, len(raw.encode()) - 1)

    def test_preserves_leading_whitespace_no_newline_markers_and_metadata(self):
        first = "\n" + patch("one.go") + "\\ No newline at end of file\n"
        second = "diff --git a/a.go b/b.go\nsimilarity index 100%\nrename from a.go\nrename to b.go\n"
        self.assertEqual(split_diff(first + second, len(first.encode())), [first, second])

    def test_content_that_mentions_diff_header_does_not_split(self):
        raw = patch("one.go", "diff --git a/example b/example")
        self.assertEqual(split_diff(raw, 1000), [raw])

    def test_oversized_later_patch_rejects_the_whole_input(self):
        raw = patch("one.go") + patch("two.go", "x" * 1000)
        with self.assertRaisesRegex(DiffBatchError, "individual file patch"):
            split_diff(raw, 200)

    def test_invalid_limits_and_missing_patches_are_rejected(self):
        for limit in (0, -1, True, "200"):
            with self.subTest(limit=limit), self.assertRaises(DiffBatchError):
                split_diff(patch("one.go"), limit)
        for raw in ("", " ", "not a diff", "unexpected\n" + patch("one.go")):
            with self.subTest(raw=raw), self.assertRaises(DiffBatchError):
                split_diff(raw, 1000)


if __name__ == "__main__":
    unittest.main()
