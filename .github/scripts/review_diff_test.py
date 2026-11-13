#!/usr/bin/env python3
"""Tests for complete review batching; no model or network required."""

from __future__ import annotations

import json
import pathlib
import sys
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).parent))
from review_diff import DiffBatchError, diff_lines, split_diff


def patch(path: str, content: str = "new") -> str:
    return f"diff --git a/{path} b/{path}\n--- a/{path}\n+++ b/{path}\n@@ -1 +1 @@\n-old\n+{content}\n"


class SplitDiffTests(unittest.TestCase):
    def test_diff_records_use_only_lf_separators(self):
        for text in ("", "\n", "a\n\n", "a\nb", "a\r\n", "a\r\v\f\x1c\x1d\x1e\x85\u2028\u2029b"):
            with self.subTest(text=text):
                records = diff_lines(text, keepends=True)
                self.assertEqual("".join(records), text)
                self.assertEqual(len(records), text.count("\n") + bool(text and not text.endswith("\n")))
                self.assertEqual(diff_lines(text), [line.removesuffix("\n") for line in records])

    def test_small_diff_is_unchanged(self):
        raw = patch("one.go") + patch("two.go")
        self.assertEqual([b.text for b in split_diff(raw, len(raw.encode()))], [raw])

    def test_batches_keep_entire_files_and_all_bytes_in_order(self):
        patches = [patch(f"file{i}.go") for i in range(5)]
        limit = len((patches[0] + patches[1]).encode())
        batches = split_diff("".join(patches), limit)
        self.assertEqual([b.text for b in batches], ["".join(patches[:2]), "".join(patches[2:4]), patches[4]])
        self.assertEqual("".join(b.text for b in batches), "".join(patches))
        self.assertTrue(all(len(batch.rendered.encode()) <= limit for batch in batches))

    def test_unicode_counts_bytes_not_characters(self):
        patches = [patch("one.go", "é" * 10), patch("two.go", "é" * 10)]
        raw = "".join(patches)
        self.assertEqual([b.text for b in split_diff(raw, len(raw))], patches)

    def test_exact_limit_is_accepted(self):
        raw = patch("one.go", "é")
        self.assertEqual([b.text for b in split_diff(raw, len(raw.encode()))], [raw])
        with self.assertRaisesRegex(DiffBatchError, "No partial review"):
            split_diff(raw, len(raw.encode()) - 1)

    def test_preserves_leading_whitespace_no_newline_markers_and_metadata(self):
        first = "\n" + patch("one.go") + "\\ No newline at end of file\n"
        second = "diff --git a/a.go b/b.go\nsimilarity index 100%\nrename from a.go\nrename to b.go\n"
        self.assertEqual([b.text for b in split_diff(first + second, len(first.encode()))], [first, second])

    def test_content_that_mentions_diff_header_does_not_split(self):
        raw = patch("one.go", "diff --git a/example b/example")
        self.assertEqual([b.text for b in split_diff(raw, 1000)], [raw])

    def assert_complete(self, raw, limit):
        batches = split_diff(raw, limit)
        self.assertEqual("".join(batch.text for batch in batches), raw)
        cursor = 0
        for batch in batches:
            self.assertEqual(batch.source_start, cursor)
            self.assertEqual(raw.encode()[batch.source_start:batch.source_end], batch.text.encode())
            self.assertLessEqual(len(batch.rendered.encode()), limit)
            cursor = batch.source_end
        self.assertEqual(cursor, len(raw.encode()))
        return batches

    def test_oversized_files_keep_every_byte_between_whole_file_batches(self):
        large = patch("large.go", "x" * 5000)
        raw = patch("before.go") + large + patch("after.go")
        batches = self.assert_complete(raw, 1000)
        self.assertEqual(batches[0].text, patch("before.go"))
        self.assertEqual(batches[-1].text, patch("after.go"))
        self.assertTrue(all(batch.context for batch in batches[1:-1]))

    def test_multimegabyte_minified_line_is_split_without_losing_unicode(self):
        raw = patch("desktop_app.html", "var π='é😀';" * 200_000) + "\\ No newline at end of file\n"
        batches = self.assert_complete(raw, 200_000)
        self.assertGreater(len(batches), 10)
        self.assertTrue(any(batch.start_column for batch in batches))
        for batch in batches[1:]:
            details = json.loads(batch.context.split(": ", 1)[1])
            self.assertIn("desktop_app.html", details["file_header"])
            self.assertEqual(details["hunk_header"], "@@ -1 +1 @@")
            self.assertEqual(details["head_line"], 1)
            self.assertEqual(details["line_prefix"], "+")
            self.assertGreater(details["line_byte_offset"], 0)

    def test_hunk_and_line_context_tracks_multiple_hunks(self):
        raw = patch("many.go", "first") + "@@ -40,0 +41,1000 @@\n" + "+next\n" * 1000
        batches = self.assert_complete(raw, 1000)
        for batch in batches[1:]:
            self.assertEqual(batch.start_column, 0)
            details = json.loads(batch.context.split(": ", 1)[1])
            self.assertEqual(details["hunk_header"], "@@ -40,0 +41,1000 @@")
            self.assertGreater(details["head_line"], 41)
            self.assertEqual(details["base_line"], 40)

    def test_every_multibyte_boundary_and_crlf_are_lossless(self):
        raw = patch("utf8.go", "éπ😀" * 300).replace("\n", "\r\n")
        for limit in range(500, 520):
            with self.subTest(limit=limit):
                self.assert_complete(raw, limit)

    def test_source_separators_do_not_change_fragment_line_metadata(self):
        for separator in ("\r", "\v", "\f", "\x1c", "\x1d", "\x1e", "\x85", "\u2028", "\u2029"):
            with self.subTest(separator=separator):
                raw = patch("separators.js", ("before" + separator + "+after") * 1000)
                batches = self.assert_complete(raw, 1000)
                self.assertGreater(len(batches), 2)
                for batch in batches[1:]:
                    details = json.loads(batch.context.split(": ", 1)[1])
                    self.assertEqual(batch.start_line, 5)
                    self.assertEqual(details["head_line"], 1)
                    self.assertEqual(details["line_prefix"], "+")
                    self.assertEqual(details["hunk_header"], "@@ -1 +1 @@")
                    self.assertGreater(batch.start_column, 0)

    def test_context_that_cannot_fit_fails_before_returning_batches(self):
        raw = patch("before.go") + patch("large.go", "x" * 1000)
        with self.assertRaisesRegex(DiffBatchError, "context.*No partial review"):
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
