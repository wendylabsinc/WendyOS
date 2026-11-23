#!/usr/bin/env python3
"""Diff reporting checks using actual Git patches and instrumented Go output."""
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import unittest

import E2ECoverage as coverage
import E2EDiff as diff


BASE = '''package sample
func Branch(covered bool) int {
    if covered {
        value := 1
        // comment inside a covered block
        value++
        return value
    }
    return 0
}
'''


class DiffCoverageTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="e2e-diff-test-")
        self.addCleanup(self.temp.cleanup)
        self.repo = Path(self.temp.name).resolve()
        self.run = self.repo / "run"
        self.file = self.repo / "go/internal/sample/sample.go"
        self.file.parent.mkdir(parents=True)
        self.file.write_text(BASE)
        (self.repo / "go/cmd/wendy").mkdir(parents=True)
        (self.repo / "go/cmd/wendy/main.go").write_text('''package main
import ("os"; "example.test/diff/go/internal/sample")
func main() { sample.Branch(os.Args[1] == "covered") }
''')
        (self.repo / "go.mod").write_text("module example.test/diff\n\ngo 1.24\n")
        (self.repo / ".gitignore").write_text("/run/\n/wendy\n")
        diff.git(self.repo, "init", "-q")
        self.base = self.commit()

    def commit(self):
        diff.git(self.repo, "add", ".")
        diff.git(self.repo, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid",
                 "-c", "commit.gpgsign=false", "commit", "-qm", "fixture", "--allow-empty")
        return diff.git(self.repo, "rev-parse", "HEAD").strip()

    def build(self):
        self.current = self.commit()
        coverage.prepare(self.repo, self.run, False)
        coverage.command("go", "build", "-cover", "-covermode=atomic", "-o", str(self.repo / "wendy"),
                         "./go/cmd/wendy", cwd=self.repo)
        native = self.run / "native"
        native.mkdir()
        subprocess.run([str(self.repo / "wendy"), "covered"], env=dict(os.environ, GOCOVERDIR=str(native)), check=True)
        source = json.loads((self.run / "coverage/source.json").read_text())
        summary = coverage.profile([native], self.run / "coverage", source)
        coverage.write_json(self.run / "coverage/summary.json", {"status": "collected", "summary": summary})
        return diff.git(self.repo, "diff", self.base, self.current)

    def compare(self, patch):
        return diff.compare(self.repo, self.run, self.base, self.current, patch)

    def test_zero_count_changed_block_is_zero_percent(self):
        self.file.write_text(BASE.replace("return 0", "return 7"))
        result = self.compare(self.build())
        self.assertEqual(result["status"], "collected")
        self.assertEqual(result["percent"], 0)
        self.assertEqual(result["statements"], 1)

    def test_one_covered_block_counted_once_across_hunks(self):
        expression = "        value := 1 +\n" + "\n".join(f"            {n} +" for n in range(2, 14)) + "\n            14"
        source = BASE.replace("        value := 1\n        // comment inside a covered block\n        value++", expression)
        self.file.write_text(source)
        self.base = self.commit()
        self.file.write_text(source.replace("value := 1 +", "value := 2 +").replace("            14", "            15"))
        patch = self.build()
        self.assertEqual(patch.count("@@ -"), 2)
        result = self.compare(patch)
        self.assertEqual(result["blocks"], 1)
        self.assertEqual(result["percent"], 100)

    def test_comment_blank_inline_comment_and_formatting_only_changes_are_not_code(self):
        for source in (BASE.replace("// comment", "// changed comment"),
                       BASE.replace("        value++", "\n        value++"),
                       BASE.replace("value := 1", "value := 1 // extra comment"),
                       BASE.replace("value := 1", "value:=1")):
            with self.subTest(source=source):
                before, after = diff.go_lines(self.repo, [{"Path": "before.go", "Source": BASE},
                                                         {"Path": "after.go", "Source": source}])
                edits = [{"removedLines": list(range(1, len(before["Lines"])+1)),
                          "addedLines": list(range(1, len(after["Lines"])+1))}]
                self.assertEqual(diff.changed_executable_lines(before, after, edits), set())

    def test_added_duplicate_uncalled_body_remains_zero_hit_changed_code(self):
        self.file.write_text(BASE.replace("func Branch", "func Added") + "\n" + BASE.split("\n", 1)[1])
        result = self.compare(self.build())  # The real CLI invokes Branch only.
        self.assertEqual(result["status"], "collected")
        self.assertEqual(result["statements"], 5)
        self.assertEqual(result["covered"], 0)
        self.assertEqual(result["percent"], 0)

    def test_raw_literal_interior_lines_select_their_uncovered_block(self):
        source = '''package sample
func Branch(covered bool) int {
    if covered { return 1 }
    value := `first
original
last`
    return len(value)
}
'''
        self.file.write_text(source)
        self.base = self.commit()
        self.file.write_text(source.replace("original", "changed"))
        result = self.compare(self.build())
        self.assertEqual(result["status"], "collected")
        self.assertEqual(result["statements"], 2)
        self.assertEqual(result["covered"], 0)
        self.assertEqual(result["percent"], 0)

    def test_comment_change_does_not_select_a_separate_block_beside_real_change(self):
        self.file.write_text(BASE.replace("value := 1", "value := 1 // extra comment").replace("return 0", "return 8"))
        result = self.compare(self.build())
        self.assertEqual(result["percent"], 0)
        self.assertEqual(result["blocks"], 1)

    def test_missing_eligible_source_never_shrinks_denominator_silently(self):
        unlinked = self.repo / "go/internal/unlinked/new.go"
        unlinked.parent.mkdir()
        unlinked.write_text("package unlinked\nfunc New() int { return 1 }\n")
        result = self.compare(self.build())
        self.assertEqual(result["status"], "incomplete")
        self.assertIsNone(result["percent"])
        self.assertEqual(result["missingFiles"], ["go/internal/unlinked/new.go"])

    def test_simulator_and_scripts_production_source_are_not_hidden(self):
        paths = ["go/simulator/production.go", "go/scripts/production.go"]
        for name in paths:
            path = self.repo / name
            path.parent.mkdir(parents=True)
            path.write_text("package production\nfunc New() int { return 1 }\n")
        result = self.compare(self.build())
        self.assertEqual(result["status"], "incomplete")
        self.assertEqual(result["missingFiles"], sorted(paths))
        self.assertIsNone(result["percent"])

    def test_literal_filtered_diff_does_not_expand_to_omitted_hunks(self):
        self.file.write_text(BASE.replace("value := 1", "value := 2").replace("return 0", "return 9"))
        self.build()
        patch = diff.git(self.repo, "diff", "--unified=0", self.base, self.current)
        sections = re.split(r"(?m)(?=^@@ )", patch)
        selected = sections[0] + sections[-1]
        self.assertIn("+    return 9", selected)
        self.assertNotIn("+        value := 2", selected)
        result = self.compare(selected)
        self.assertEqual(result["percent"], 0)
        self.assertEqual(result["blocks"], 1)
        self.assertEqual(result["provenance"]["paths"], ["go/internal/sample/sample.go"])

    def test_deleted_statements_do_not_enter_changed_denominator(self):
        self.file.write_text(BASE.replace("        value++\n", ""))
        self.assertEqual(self.compare(self.build())["status"], "not-applicable")

    def test_excluded_test_generated_nonproduction_and_declarations(self):
        for name, text in {
            "go/internal/sample/subject_test.go": "package sample\nfunc OnlyTest() int {return 9}\n",
            "go/internal/sample/generated.go": "// Code generated by fixture. DO NOT EDIT.\npackage sample\nfunc Generated() int {return 9}\n",
            "go/internal/sample/attribute.go": "package sample\nfunc Attribute() int {return 9}\n",
            "go/internal/sample/testdata/helper.go": "package testdata\nfunc Fixture() int {return 9}\n",
            "Examples/main.go": "package main\nfunc main() {}\n",
        }.items():
            path = self.repo / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(text)
        (self.repo / ".gitattributes").write_text("go/internal/sample/attribute.go linguist-generated=true\n")
        self.file.write_text(BASE + "\ntype Declaration int\n")
        result = self.compare(self.build())
        self.assertEqual(result["status"], "not-applicable")
        self.assertEqual(result["blocks"], 0)

    def test_pure_rename_and_empty_diff_are_not_applicable(self):
        renamed = self.file.with_name("renamed.go")
        self.file.rename(renamed)
        patch = self.build()
        self.assertIn("similarity index 100%", patch)
        self.assertEqual(self.compare(patch)["status"], "not-applicable")
        result = diff.compare(self.repo, self.run, self.current, self.current, "")
        self.assertEqual(result["status"], "not-applicable")

    def test_malformed_wrong_source_and_wrong_patch_fail_clearly(self):
        self.file.write_text(BASE.replace("return 0", "return 7"))
        patch = self.build()
        with self.assertRaisesRegex(ValueError, "unified diff"):
            self.compare("garbage")
        with self.assertRaisesRegex(ValueError, "source revision"):
            diff.compare(self.repo, self.run, self.base, self.base, patch)
        with self.assertRaisesRegex(ValueError, "does not match"):
            self.compare(patch.replace("+    return 7", "+    return 999"))
        self.assertEqual(diff.git(self.repo, "status", "--porcelain"), "")

    def test_informational_failures_and_profile_regeneration_keep_overall_honest(self):
        self.file.write_text(BASE.replace("return 0", "return 7"))
        patch = self.build()
        native = self.run / "observations/suite/test/coverage/go"
        native.parent.mkdir(parents=True)
        (self.run / "native").rename(native)
        source = json.loads((self.run / "coverage/source.json").read_text())
        coverage.write_json(self.run / "coverage/tests.json", [{"test": "fixture", "status": "collected",
                            "sourceID": source["sourceID"], "directory": "observations/suite/test"}])
        coverage.write_json(self.run / "attempt.json", {"exitStatus": 0})
        self.assertEqual(coverage.report(self.repo, self.run), 0)
        total = (self.run / "coverage/coverage.out").read_bytes()
        patch_file = self.run / "input.patch"
        patch_file.write_text(patch)
        tool = Path(diff.__file__)
        def invoke(*args):
            result = subprocess.run([sys.executable, str(tool), "--repo", str(self.repo),
                                     "--run-dir", str(self.run), *args], capture_output=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual((self.run / "coverage/coverage.out").read_bytes(), total)
            return json.loads((self.run / "coverage/diff.json").read_text())
        invalid = invoke("--base-ref", "missing-base")
        self.assertEqual(invalid["status"], "unavailable")
        self.assertIsNone(invalid["percent"])
        valid = invoke("--diff", str(patch_file), "--base-revision", self.base, "--source-revision", self.current)
        self.assertEqual(valid["percent"], 0)
        self.assertEqual(coverage.report(self.repo, self.run, per_test=True), 0)
        summary = json.loads((self.run / "coverage/summary.json").read_text())
        self.assertEqual(summary["diff"]["status"], "collected")
        self.assertEqual((self.run / "coverage/coverage.out").read_bytes(), total)
        (self.run / "coverage/diff.json").write_text("malformed")
        self.assertEqual(coverage.report(self.repo, self.run), 0)
        self.assertEqual(json.loads((self.run / "coverage/summary.json").read_text())["diff"]["status"], "unavailable")


if __name__ == "__main__":
    unittest.main()
