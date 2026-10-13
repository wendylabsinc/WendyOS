#!/usr/bin/env python3
"""Tests for complete review batching; no model or network required."""

from __future__ import annotations

import pathlib
import re
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
        with self.assertRaises(DiffBatchError):
            split_diff(raw, len(raw.encode()) - 1)

    def test_preserves_leading_whitespace_no_newline_markers_and_metadata(self):
        first = "\n" + patch("one.go") + "\\ No newline at end of file\n"
        second = "diff --git a/a.go b/b.go\nsimilarity index 100%\nrename from a.go\nrename to b.go\n"
        self.assertEqual(split_diff(first + second, len(first.encode())), [first, second])

    def test_content_that_mentions_diff_header_does_not_split(self):
        raw = patch("one.go", "diff --git a/example b/example")
        self.assertEqual(split_diff(raw, 1000), [raw])

    def test_oversized_later_patch_is_fragmented_without_losing_source(self):
        large = patch("two.go", "é🙂" * 1000)
        batches = split_diff(patch("one.go") + large, 500)
        self.assertEqual(batches[0], patch("one.go"))
        self.assertTrue(all(len(batch.encode()) <= 500 for batch in batches))
        self.assert_source_fragments(large, batches[1:])

    def assert_source_fragments(self, original, batches):
        original_lines = original.splitlines()
        assembled = {}
        marker = re.compile(r"^@@ .* review fragment: UTF-8 bytes (\d+):(\d+)/(\d+) of original diff line (\d+)$")
        for batch in batches:
            self.assertTrue(batch.startswith(original.split("@@", 1)[0]))
            lines = batch.splitlines()
            for i, line in enumerate(lines):
                match = marker.fullmatch(line)
                if not match:
                    continue
                start, end, total, source_line = map(int, match.groups())
                body = lines[i + 1]
                self.assertEqual(body[:1], original_lines[source_line - 1][:1])
                previous = assembled.get(source_line, b"")
                self.assertEqual(start, len(previous))
                chunk = body[1:].encode()
                self.assertEqual(len(chunk), end - start)
                assembled[source_line] = previous + chunk
                self.assertEqual(total, len(original_lines[source_line - 1][1:].encode()))
        for source_line, content in assembled.items():
            self.assertEqual(content, original_lines[source_line - 1][1:].encode())
        expected = [i for i, line in enumerate(original_lines, 1)
                    if line[:1] in ("+", "-", " ") and not line.startswith(("---", "+++"))]
        self.assertEqual(list(assembled), expected)

    def test_fragments_keep_source_numbers_for_deletions_insertions_and_context(self):
        raw = ("diff --git a/ui.html b/ui.html\nindex abc..def 100644\n--- a/ui.html\n+++ b/ui.html\n"
               "@@ -41,2 +51,2 @@ section\n-" + "old" * 1000 + "\n+" + "new" * 1000
               + "\n context\n\\ No newline at end of file\n")
        batches = split_diff(raw, 500)
        self.assert_source_fragments(raw, batches)
        self.assertIn("@@ -41,1 +51,0 @@", batches[0])
        self.assertTrue(any("@@ -42,0 +51,1 @@" in batch for batch in batches))
        self.assertIn("\\ No newline at end of file\n", batches[-1])

    def test_unrepresentable_metadata_and_malformed_hunks_fail_closed(self):
        for raw in (patch("x" * 500, "new"), patch("one.go", "x" * 1000).replace("@@ -1 +1 @@", "@@ -2,2 +2 @@")):
            with self.subTest(raw=raw[:80]), self.assertRaises(DiffBatchError):
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
