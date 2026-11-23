#!/usr/bin/env python3
"""Informational diff coverage and source snapshots for E2E spec badges.

The input is an ordinary Git patch with known base/current revisions. Git
validates its contents; Go's parser/scanner distinguish code from comments.
Coverage counts whole intersecting Go blocks, never individual changed lines.
"""

import argparse
import codecs
import difflib
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile

from E2ECoverage import command, source_id, write_json


GO_LINES = r'''package main
import ("crypto/sha256"; "encoding/hex"; "encoding/json"; "go/ast"; "go/parser"; "go/scanner"; "go/token"; "os"; "strings")
type Input struct { Path, Source string }
type Output struct { Lines [][]string; Executable []int }
func main() {
    var inputs []Input
    if err := json.NewDecoder(os.Stdin).Decode(&inputs); err != nil { panic(err) }
    outputs := []Output{}
    for _, input := range inputs {
        fs := token.NewFileSet()
        tree, err := parser.ParseFile(fs, input.Path, input.Source, 0)
        if err != nil { panic(err) }
        spans := [][2]token.Pos{}
        ast.Inspect(tree, func(n ast.Node) bool {
            if s, ok := n.(ast.Stmt); ok {
                switch s.(type) {
                case *ast.BlockStmt, *ast.EmptyStmt, *ast.CaseClause, *ast.CommClause, *ast.LabeledStmt:
                default: spans = append(spans, [2]token.Pos{s.Pos(), s.End()})
                }
            }
            return true
        })
        out := Output{Lines: make([][]string, strings.Count(input.Source, "\n")+1), Executable: []int{}}
        file := fs.File(tree.Pos())
        var scan scanner.Scanner
        scan.Init(file, []byte(input.Source), nil, 0)
        eligible := map[int]bool{}
        for {
            pos, kind, literal := scan.Scan()
            if kind == token.EOF { break }
            if kind == token.SEMICOLON { continue }
            line := fs.Position(pos).Line
            if kind == token.LBRACE || kind == token.RBRACE { continue }
            spelling := kind.String()+"\x00"+literal
            if strings.Contains(literal, "\n") {
                sum := sha256.Sum256([]byte(literal))
                spelling = kind.String()+"\x00"+hex.EncodeToString(sum[:])
            }
            for _, span := range spans {
                if pos >= span[0] && pos < span[1] {
                    eligible[line] = true
                    for n := line; n <= line+strings.Count(literal, "\n"); n++ {
                        out.Lines[n-1] = append(out.Lines[n-1], spelling)
                    }
                    break
                }
            }
        }
        for line := 1; line <= len(out.Lines); line++ {
            if eligible[line] { out.Executable = append(out.Executable, line) }
        }
        outputs = append(outputs, out)
    }
    if err := json.NewEncoder(os.Stdout).Encode(outputs); err != nil { panic(err) }
}
'''


def git(repo, *args, **kwargs):
    return command("git", *args, cwd=repo, **kwargs)


def blob(repo, revision, path):
    result = subprocess.run(["git", "show", f"{revision}:{path}"], cwd=repo, capture_output=True)
    return result.stdout.decode() if result.returncode == 0 else None


def revision(repo, value):
    if not value or value.startswith("-"):
        raise ValueError("A valid comparison revision is required")
    return git(repo, "rev-parse", "--verify", value + "^{commit}").strip()


def patch_paths(repo, patch):
    if patch.strip() and not patch.startswith("diff --git "):
        raise ValueError("Expected an ordinary Git unified diff")
    raw = git(repo, "apply", "--numstat", "-z", "--allow-empty", input=patch)
    paths = [row.split("\t", 2)[2] for row in raw.split("\0") if row]
    if patch.strip() and not paths:
        raise ValueError("Malformed diff: no file changes")
    return paths


def checked_patch(repo, patch, base, current):
    paths = patch_paths(repo, patch)
    renamed = renamed_paths(repo, base, current)
    selected = {}
    # Validate literal hunk positions/content in BOTH revisions. This permits
    # filtered patches and never expands an omitted hunk to the whole file.
    for part in re.split(r"(?m)(?=^diff --git )", patch):
        if not part.strip():
            continue
        names = patch_paths(repo, part)
        if len(names) != 1:
            raise ValueError("Expected one file per Git diff section")
        path = names[0]
        if not path.endswith((".go", ".swift")):
            continue
        old_path = renamed.get(path, path)
        header = next((line[4:].rstrip("\t") for line in part.splitlines() if line.startswith("--- ")), None)
        if header and header != "/dev/null":
            if header.startswith('"'):
                header = codecs.escape_decode(header[1:-1].encode())[0].decode()
            if not header.startswith("a/"):
                raise ValueError("Expected Git a/ source path")
            old_path = header[2:]
        old = (blob(repo, base, old_path) or "").splitlines()
        new = (blob(repo, current, path) or "").splitlines()
        added, removed = set(), set()
        lines = part.splitlines()
        i = 0
        while i < len(lines):
            match = re.match(r"^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@", lines[i])
            i += 1
            if not match:
                continue
            old_start, old_count, new_start, new_count = match.groups()
            a, b = int(old_start), int(new_start)
            old_end, new_end = a + int(old_count or 1), b + int(new_count or 1)
            while i < len(lines) and not lines[i].startswith("@@ "):
                line = lines[i]
                i += 1
                if line.startswith("\\ No newline"):
                    continue
                if not line or line[0] not in " +-":
                    raise ValueError("Malformed diff hunk: " + path)
                if line[0] in " -":
                    if a < 1 or a > len(old) or old[a-1] != line[1:]:
                        raise ValueError("Diff does not match the base revision: " + path)
                    if line[0] == "-": removed.add(a)
                    a += 1
                if line[0] in " +":
                    if b < 1 or b > len(new) or new[b-1] != line[1:]:
                        raise ValueError("Diff does not match the source revision: " + path)
                    if line[0] == "+": added.add(b)
                    b += 1
            if a != old_end or b != new_end:
                raise ValueError("Incorrect diff hunk counts: " + path)
        selected[path] = {"oldPath": old_path, "addedLines": sorted(added), "removedLines": sorted(removed)}
    return paths, selected


def renamed_paths(repo, base, current):
    fields = git(repo, "diff", "--name-status", "-z", "--find-renames", base, current).split("\0")
    renamed = {}
    while fields and fields[0]:
        status = fields.pop(0)
        old = fields.pop(0)
        if status.startswith(("R", "C")):
            renamed[fields.pop(0)] = old
    return renamed


def production_go(repo, path, text, current, source):
    if not path.startswith("go/") or not path.endswith(".go") or path.endswith("_test.go"):
        return False
    # Go itself excludes testdata from package builds. Product directories such
    # as simulator/scripts are deliberately NOT excluded by their names.
    if "testdata" in Path(path).parts:
        return False
    if Path(path).name == "coverage_e2e.go":
        return False
    if re.search(r"(?m)^// Code generated .* DO NOT EDIT\.$", text.split("\npackage ", 1)[0]):
        return False
    if git(repo, "check-attr", "--source=" + current, "linguist-generated", "--", path).rstrip().split(": ")[-1] in ("true", "set"):
        return False
    return not any(f["path"] == path and (f["generated"] or f["testHarness"])
                   for f in source["files"].values())


def go_lines(repo, inputs):
    with tempfile.TemporaryDirectory(prefix="e2e-diff-go-") as directory:
        helper = Path(directory) / "main.go"
        helper.write_text(GO_LINES)
        return json.loads(command("go", "run", str(helper), cwd=repo, input=json.dumps(inputs)))


def changed_executable_lines(before, after):
    # Compare non-comment token lines, so blank/comment-only changes inside a
    # covered block cannot select it. Whole-file token equality also ignores
    # pure renames and formatting-only changes (including line wrapping).
    old = [(i + 1, tuple(v)) for i, v in enumerate(before["Lines"]) if v]
    new = [(i + 1, tuple(v)) for i, v in enumerate(after["Lines"]) if v]
    if [t for _, ts in old for t in ts] == [t for _, ts in new for t in ts]:
        return set()
    matcher = difflib.SequenceMatcher(a=[v for _, v in old], b=[v for _, v in new], autojunk=False)
    changed = {new[i][0] for op, _, _, start, end in matcher.get_opcodes()
               if op != "equal" for i in range(start, end)}
    return changed & set(after["Executable"])


def compare(repo, run, base, current, patch):
    source = json.loads((run / "coverage/source.json").read_text())
    coverage = json.loads((run / "coverage/summary.json").read_text())
    base, current = revision(repo, base), revision(repo, current)
    if current != source["revision"]:
        raise ValueError("Diff source revision differs from the coverage build")
    if source_id(repo, [f["path"] for f in source["files"].values()]) != source["sourceID"]:
        raise ValueError("Checkout Go source differs from the coverage build")
    if git(repo, "diff", current, "--", "*.go", "go.mod", "go.sum").strip():
        raise ValueError("Diff requires the recorded committed Go source")
    if git(repo, "ls-files", "--others", "--exclude-standard", "--", "*.go").strip():
        raise ValueError("Uncommitted Go files have no comparison revision")
    git(repo, "ls-files", "--error-unmatch", "--", *[f["path"] for f in source["files"].values()])
    paths, selected_lines = checked_patch(repo, patch, base, current)
    renamed = renamed_paths(repo, base, current)
    provenance = {"baseRevision": base, "sourceRevision": current, "sourceID": source["sourceID"],
                  "patchSHA256": hashlib.sha256(patch.encode()).hexdigest(), "paths": paths}
    result = {"provenance": provenance, "files": [], "granularity": "Statements in intersecting Go coverage blocks; each block counted once"}
    inputs, selected = [], []
    for path in paths:
        if not path.endswith((".go", ".swift")):
            continue
        old_path = selected_lines[path]["oldPath"]
        before, after = blob(repo, base, old_path), blob(repo, current, path)
        if path.startswith("swift/WendyE2ETests/Tests/WendyE2ETests/") and path.endswith(".swift"):
            result["files"].append({"path": path, "oldPath": old_path, "baseSource": before,
                                    "sourceFilePath": path.removeprefix("swift/WendyE2ETests/"),
                                    "source": after, **selected_lines[path]})
        if after is None or not production_go(repo, path, after, current, source):
            continue
        inputs.extend([{"Path": old_path, "Source": before or "package missing\n"}, {"Path": path, "Source": after}])
        selected.append(path)
    parsed = go_lines(repo, inputs) if inputs else []
    changed = {p: changed_executable_lines(parsed[i*2], parsed[i*2+1])
               & set(selected_lines[p]["addedLines"]) for i, p in enumerate(selected)}
    changed = {p: lines for p, lines in changed.items() if lines}
    profile = run / "coverage/coverage.out"
    provenance["profileSHA256"] = hashlib.sha256(profile.read_bytes()).hexdigest() if profile.exists() else None
    blocks, present = {}, set()
    for line in profile.read_text().splitlines()[1:] if profile.exists() else []:
        location, statements, count = line.rsplit(" ", 2)
        name, coordinates = location.rsplit(":", 1)
        path = source["files"][name]["path"]
        present.add(path)
        start, end = coordinates.split(",")
        first, last = int(start.split(".")[0]), int(end.split(".")[0])
        if int(end.split(".")[1]) == 1:
            last -= 1
        if int(statements) and any(first <= n <= last for n in changed.get(path, ())):
            blocks[location] = (int(statements), int(count))
    missing = sorted(set(changed) - present)
    statements = sum(n for n, _ in blocks.values())
    covered = sum(n for n, count in blocks.values() if count > 0)
    status = "not-applicable" if not changed else "incomplete" if missing or coverage["status"] != "collected" else "collected" if statements else "not-applicable"
    result.update(status=status, missingFiles=missing, statements=statements, covered=covered, blocks=len(blocks),
                  percent=100*covered/statements if status == "collected" else None)
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repo", type=Path, default=Path(__file__).resolve().parents[2])
    parser.add_argument("--run-dir", type=Path, required=True)
    parser.add_argument("--base-ref", help="PR adapter: compare the merge base of this ref and the tested revision")
    parser.add_argument("--diff", type=Path, help="Ordinary Git unified diff; requires --base-revision and --source-revision")
    parser.add_argument("--base-revision")
    parser.add_argument("--source-revision")
    args = parser.parse_args()
    repo, run = args.repo.resolve(), args.run_dir.resolve()
    patch = None
    try:
        if args.base_ref:
            if args.diff or args.base_revision or args.source_revision:
                raise ValueError("Choose --base-ref or an explicit diff and both revisions")
            current = json.loads((run / "coverage/source.json").read_text())["revision"]
            base = git(repo, "merge-base", revision(repo, args.base_ref), current).strip()
            patch = git(repo, "diff", "--no-ext-diff", "--no-color", base, current)
        else:
            if not args.diff or not args.base_revision or not args.source_revision:
                raise ValueError("Provide --base-ref, or --diff with --base-revision and --source-revision")
            base, current, patch = args.base_revision, args.source_revision, args.diff.read_text()
        result = compare(repo, run, base, current, patch)
    except Exception as error:
        result = {"status": "unavailable", "reason": str(error), "percent": None, "files": []}
        print("E2E diff coverage unavailable:", error)
    if patch is not None:
        (run / "coverage/diff.patch").write_text(patch)
    write_json(run / "coverage/diff.json", result)
    for path in (run / "attempt.json", run / "coverage/summary.json"):
        if path.exists():
            metadata = json.loads(path.read_text())
            destination = metadata.setdefault("coverage", {}) if path.name == "attempt.json" else metadata
            destination["diff"] = {k: v for k, v in result.items() if k != "files"}
            write_json(path, metadata)


if __name__ == "__main__":
    main()
