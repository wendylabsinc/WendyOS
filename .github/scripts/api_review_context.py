"""Bounded nearby source context for API Review, never executable PR code."""
from __future__ import annotations

from collections import defaultdict, deque
import pathlib
import re
import subprocess

from review_diff import diff_lines
from review_sources import is_generated_path, valid_path

MAX_CONTEXT_BYTES = 48_000
MAX_FILE_BYTES = 8_000
MAX_BLOB_BYTES = 1_000_000
MAX_FILES = 12
MAX_ANCHORS = 64
SOURCE_SUFFIXES = {".go", ".swift", ".proto", ".py", ".sh", ".json", ".yaml", ".yml", ".toml"}
COMMON = set("func function static public private internal nonisolated return throw throws guard case else true false bool string int var let const type class struct enum error errors client server service request response context logger test tests package import self nil main source file value name info get set make new check default config".split())
BOUNDARY = set("auth authorization authentication cert certificate credential credentials identity permission scope role capability version fallback default".split())
BRANCH = re.compile(r"\b(if|guard|switch|case|throw|raise|return)\b")
FUNCTION = re.compile(r"^\s*(?:(?:public|private|internal|static|nonisolated|final|async)\s+)*(?:func|def|function)\s+(?:\([^)]*\)\s*)?([A-Za-z_][A-Za-z0-9_]*)")
LIMITATION = (
    "Partial context: production changes and identifier-matched neighbors only in changed directories, "
    "shared across languages. Tests remain in the diff but do not consume supplemental slots. "
    "Large files use bounded branch/function neighborhoods and local reference excerpts, not full files. "
    "No repository-wide text search or caller graph; no release/deployment inventory. External schemas are not fetched. "
    "Missing callers, schemas or retirement evidence mean insufficient evidence, not compatibility."
)


def git(repository: str, *args: str) -> bytes:
    return subprocess.run(["git", "-C", repository, *args], check=True, capture_output=True,
                          timeout=30).stdout


def words(source: str) -> set[str]:
    return {word.lower() for identifier in re.findall(r"[A-Za-z][A-Za-z0-9_]*", source)
            for word in [identifier, *re.findall(r"[A-Z]+(?=[A-Z][a-z]|$)|[A-Z]?[a-z]+|[0-9]+", identifier)]
            if len(word) >= 3 and word.lower() not in COMMON}


def test_path(path: str) -> bool:
    p = pathlib.PurePosixPath(path)
    return (any(part.lower() in {"test", "tests", "testdata", "fixtures"} for part in p.parts)
            or p.stem.endswith(("_test", "Tests", "Test")) or p.name.startswith("test_"))


def fair(candidates, limit):
    """Round-robin by language, retaining the supplied priority within each."""
    groups = defaultdict(deque)
    for item in sorted(candidates):
        groups[pathlib.PurePosixPath(item[1]).suffix].append(item)
    chosen = []
    while len(chosen) < limit and any(groups.values()):
        for group in sorted(groups):
            if groups[group] and len(chosen) < limit:
                chosen.append(groups[group].popleft())
    return chosen


def excerpt(source: str, anchors: set[int], terms: set[str], budget: int):
    lines = source.splitlines(keepends=True)
    if len(source.encode()) <= budget:
        return source, [[1, len(lines)]]
    points = sorted(anchors)
    if len(points) > MAX_ANCHORS:
        points = [points[i * (len(points) - 1) // (MAX_ANCHORS - 1)] for i in range(MAX_ANCHORS)]
    declarations = [(i, m.group(1)) for i, line in enumerate(lines) if (m := FUNCTION.match(line))]
    scored = []
    for anchor in points:
        start, end = max(0, anchor - 21), min(len(lines), anchor + 20)
        # Include a small enclosing function, not an import-only first hunk.
        enclosing = [(i, name) for i, name in declarations if i <= anchor - 1]
        name = ""
        if enclosing:
            first, name = enclosing[-1]
            last = next((i for i, _ in declarations if i > first), len(lines))
            if last - first <= 100:
                start, end = first, last
        text = "".join(lines[start:end])
        matches = words(text) & terms
        score = len(matches) + 4 * len(matches & BOUNDARY) + 2 * len(BRANCH.findall(text))
        scored.append((-score, start, end, name))
    selected = []
    for _score, start, end, name in sorted(set(scored)):
        if any(start < b and end > a for a, b in selected):
            continue
        # At most one local reference neighborhood per selected function;
        # no recursion or traversal into other files.
        references = [i for i, line in enumerate(lines)
                      if name and re.search(r"\b" + re.escape(name) + r"\s*\(", line)
                      and not start <= i < end]
        ranges = [(start, end)]
        if references:
            i = references[0]
            ranges.append((max(0, i - 15), min(len(lines), i + 16)))
        for a, b in ranges:
            if len(selected) >= 3:
                break
            if any(a < old_b and b > old_a for old_a, old_b in selected):
                continue
            candidate = sorted([*selected, (a, b)])
            size = sum(len(f"[source lines {x+1}-{y}]\n".encode()) + len("".join(lines[x:y]).encode()) + 1 for x, y in candidate)
            if size <= budget:
                selected = candidate
        if len(selected) >= 3:
            break
    text = "\n".join(f"[source lines {a+1}-{b}]\n" + "".join(lines[a:b]) for a, b in selected)
    return text, [[a + 1, b] for a, b in selected]


def source_context(repository: str, parsed: dict, base: str, head: str, diff: str = "") -> dict:
    changed = {path for path, _side in parsed["locations"]}
    parents = {str(pathlib.PurePosixPath(path).parent) for path in changed}
    result = {"limitation": LIMITATION, "files": [], "omitted_files": 0, "bytes": 0, "partial_files": 0}
    trees = {}
    for side, revision in (("base", base), ("head", head)):
        tree = {}
        for row in git(repository, "ls-tree", "-rlz", revision).split(b"\0"):
            if not row:
                continue
            fields, name = row.split(b"\t", 1)
            mode, kind, oid, size = fields.decode("ascii").split()
            path = name.decode("utf-8", errors="replace")
            # Canonical exclusion and regular-file checks precede ALL blob reads.
            if (mode in {"100644", "100755"} and kind == "blob" and valid_path(path)
                    and not is_generated_path(path) and not test_path(path)
                    and pathlib.PurePosixPath(path).suffix in SOURCE_SUFFIXES
                    and str(pathlib.PurePosixPath(path).parent) in parents):
                tree[path] = (oid, int(size))
        trees[side] = tree
    sections = {}
    patch = diff_lines(diff)
    for i, file in enumerate(parsed.get("files", [])):
        end = parsed["files"][i + 1]["patch_line"] if i + 1 < len(parsed["files"]) else len(patch)
        sections[file["head"] or file["base"]] = patch[file["patch_line"]:end]
    terms, changes = set(), []
    for path in sorted(changed):
        side = "head" if path in trees["head"] else "base"
        if path not in trees[side]:
            continue
        code = [line[1:] for line in sections.get(path, []) if line[:1] in {"+", "-"}
                and not line.startswith(("+++", "---")) and not line[1:].lstrip().startswith(("//", "#", "*"))]
        if diff and not code:
            continue
        terms |= words("\n".join(sections.get(path, [])))
        priority = -sum(bool(BRANCH.search(line)) for line in code)
        changes.append((priority, path, side))
    related = []
    for path in trees["head"]:
        if path in changed:
            continue
        stem = pathlib.PurePosixPath(path).stem
        matches = words(stem) & terms
        if matches:
            related.append((-(len(matches) + 6 * (stem.lower() in terms) + 6 * len(matches & BOUNDARY)), path, "head"))
    # Half the slots for production changes, half for nearby unchanged callers.
    # Unused slots are shared, but a busy Go/test directory cannot evict Swift.
    selected = fair(changes, MAX_FILES // 2) + fair(related, MAX_FILES // 2)
    selected += fair([x for x in changes + related if x not in selected], MAX_FILES - len(selected))
    result["omitted_files"] = len(changes) + len(related) - len(selected)
    for _priority, path, side in selected:
        oid, size = trees[side][path]
        if size > MAX_BLOB_BYTES:
            result["omitted_files"] += 1
            continue
        raw = git(repository, "cat-file", "blob", oid)
        try:
            source = raw.decode("utf-8") if b"\0" not in raw else ""
        except UnicodeDecodeError:
            source = ""
        anchors = parsed["locations"].get((path, side), set()) - {0}
        if path not in changed:
            anchors = {i for i, line in enumerate(source.splitlines(), 1) if words(line) & terms}
        budget = min(MAX_FILE_BYTES, MAX_CONTEXT_BYTES - result["bytes"])
        if path in changed:
            budget = min(budget, MAX_CONTEXT_BYTES // MAX_FILES)
        text, ranges = excerpt(source, anchors, terms, budget)
        if not text:
            result["omitted_files"] += 1
            continue
        partial = ranges != [[1, len(source.splitlines())]]
        result["files"].append({"path": path, "side": side, "sha": head if side == "head" else base,
                                "unchanged": path not in changed, "partial": partial,
                                "start_line": ranges[0][0], "ranges": ranges,
                                "also_at_base": base if path not in changed and trees["base"].get(path) == (oid, size) else "",
                                "source": text})
        result["partial_files"] += int(partial)
        result["bytes"] += len(text.encode())
    return result
