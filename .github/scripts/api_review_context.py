"""Bounded nearby source context for API Review, never executable PR code."""
from __future__ import annotations

from collections import Counter, defaultdict, deque
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
MAX_SCAN_FILES = 64
MAX_SCAN_BYTES = 1024 * 1024
MAX_MODULE_FILES = 16
SOURCE_SUFFIXES = {".go", ".swift", ".proto", ".py", ".sh", ".json", ".yaml", ".yml", ".toml"}
COMMON = set("func function static public private internal nonisolated return throw throws guard case else true false bool string int var let const type class struct enum error errors client server service request response context logger test tests package import self nil main source file value name info get set make new check default config".split())
BOUNDARY = set("auth authorization authentication cert certificate credential credentials identity permission scope role capability version fallback default".split())
BRANCH = re.compile(r"\b(if|guard|switch|case|throw|raise|return)\b")
FUNCTION = re.compile(r"^\s*(?:(?:public|private|internal|static|nonisolated|final|async)\s+)*(?:func|def|function)\s+(?:\([^)]*\)\s*)?([A-Za-z_][A-Za-z0-9_]*)")
LIMITATION = (
    "Partial context: unseen production source, nearby contract names and exact source references in changed directories, "
    "shared across languages. Tests remain in the diff but do not consume supplemental slots. "
    "Large files use bounded branch/function neighborhoods and local reference excerpts, not full files. "
    "Source search is limited to changed-directory modules of at most 16 source files, capped at 64 files/1 MiB before the unchanged 12-file/48 KB excerpt limit. No repository-wide search or caller graph; no release/deployment inventory. External schemas are not fetched. "
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


def identifiers(source):
    # Ranking only: prose/string literals cannot outrank actual source uses.
    source = re.sub(r"/\*.*?\*/|//[^\n]*|#[^\n]*|\"(?:\\.|[^\"\\])*\"|'(?:\\.|[^'\\])*'|`[^`]*`", " ", source, flags=re.S)
    return {name for name in re.findall(r"\b[A-Za-z_][A-Za-z0-9_]{2,}\b", source)
            if name.lower() not in COMMON}


def supplied_lines(parsed, diff):
    """All visible patch lines, including unchanged context, by revision side."""
    result = defaultdict(set)
    patch = diff_lines(diff)
    for index, file in enumerate(parsed.get("files", [])):
        end = parsed["files"][index + 1]["patch_line"] if index + 1 < len(parsed["files"]) else len(patch)
        old = new = None
        for line in patch[file["patch_line"]:end]:
            match = re.match(r"@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@", line)
            if match:
                old, new = map(int, match.groups())
            elif old is not None:
                if line.startswith((" ", "-")):
                    result[(file["base"], "base")].add(old)
                    old += 1
                if line.startswith((" ", "+")):
                    result[(file["head"], "head")].add(new)
                    new += 1
    return result


def excerpt(source: str, anchors: set[int], terms: set[str], budget: int, visible=None, relevance=None):
    lines = source.splitlines(keepends=True)
    visible = visible or set()
    relevance = relevance or {}
    if lines and all(i in visible for i in range(1, len(lines) + 1)):
        return "", []
    if len(source.encode()) <= budget:
        return source, [[1, len(lines)]]
    points = sorted(anchors)
    if len(points) > MAX_ANCHORS:
        if relevance:
            points = sorted(points, key=lambda i: (-sum(relevance.get(name, 0) for name in identifiers(lines[i-1])), i))[:MAX_ANCHORS]
        else:
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
        matches = words(" ".join(identifiers(text))) & terms
        unseen = sum(i not in visible for i in range(start + 1, end + 1))
        if not unseen:
            continue
        score = (20 * sum(relevance.get(name, 0) for name in identifiers(text))
                 + len(matches) + 4 * len(matches & BOUNDARY) + min(4, len(BRANCH.findall(text))))
        score *= unseen / max(1, end - start)
        scored.append((-score, start, end, name))
    selected = []
    for _score, start, end, name in sorted(set(scored)):
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
            if all(i in visible for i in range(a + 1, b + 1)):
                continue
            candidate = []
            for first, last in sorted([*selected, (a, b)]):
                if candidate and first <= candidate[-1][1]:
                    candidate[-1] = (candidate[-1][0], max(last, candidate[-1][1]))
                else:
                    candidate.append((first, last))
            if len(candidate) > 3:
                continue
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
    result = {"limitation": LIMITATION, "files": [], "omitted_files": 0, "bytes": 0, "partial_files": 0,
              "scanned_files": 0, "scanned_bytes": 0, "scan_omitted_files": 0, "already_supplied_files": 0}
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
    # One capped lexical scan of the existing directory scope, not recursive
    # caller/import traversal. Exact source identifiers beat filename guesses.
    counts = Counter()
    for line in patch:
        if line[:1] in {"+", "-"} and not line.startswith(("+++", "---")) and not line[1:].lstrip().startswith(("//", "#", "*")):
            names = identifiers(line[1:])
            counts.update(name for name in re.findall(r"\b([A-Za-z_][A-Za-z0-9_]*)\s*(?=[({])", line[1:]) if name in names)
    queries = {name for name, _ in sorted(counts.items(), key=lambda item: (-item[1], item[0]))[:MAX_ANCHORS]}
    if diff and not changes:
        return result
    sources, hits = {}, {}
    module_sizes = Counter(str(pathlib.PurePosixPath(path).parent) for path in trees["head"])
    for _, path, _ in fair([(0, path, "head") for path in trees["head"]], len(trees["head"])):
        oid, size = trees["head"][path]
        if (module_sizes[str(pathlib.PurePosixPath(path).parent)] > MAX_MODULE_FILES
                or size > MAX_BLOB_BYTES or result["scanned_files"] >= MAX_SCAN_FILES
                or result["scanned_bytes"] + size > MAX_SCAN_BYTES):
            result["scan_omitted_files"] += 1
            continue
        raw = git(repository, "cat-file", "blob", oid)
        result["scanned_files"] += 1
        result["scanned_bytes"] += len(raw)
        try:
            source = raw.decode("utf-8") if b"\0" not in raw else ""
        except UnicodeDecodeError:
            source = ""
        sources[path] = source
        hits[path] = identifiers(source) & queries
    frequency = Counter(name for names in hits.values() for name in names)
    relevance = {name: 1 / count for name, count in frequency.items()}
    visible = supplied_lines(parsed, diff)
    changes = [row for row in changes if not (row[2] == "head" and row[1] in sources
               and all(i in visible[(row[1], "head")] for i in range(1, len(sources[row[1]].splitlines()) + 1)))]
    result["already_supplied_files"] = sum(bool(source) and all(i in visible[(path, "head")] for i in range(1, len(source.splitlines()) + 1))
                                           for path, source in sources.items())
    related = [(-sum(relevance[name] for name in names), path, "head")
               for path, names in hits.items() if names and path not in changed]
    named = []
    for path in trees["head"]:
        matches = words(pathlib.PurePosixPath(path).stem) & terms
        if path not in changed and matches:
            named.append((-(len(matches) + 6 * (pathlib.PurePosixPath(path).stem.lower() in terms) + 6 * len(matches & BOUNDARY)), path, "head"))
    # Equal bounded shares: unseen changes, nearby contract names, source uses.
    selected = fair(changes, MAX_FILES // 3) + fair(named, MAX_FILES // 3)
    selected += fair([row for row in related if row[1] not in {item[1] for item in selected}], MAX_FILES // 3)
    selected += fair([row for row in changes + related if row[1] not in {item[1] for item in selected}], MAX_FILES - len(selected))
    # Prefer compact selected modules before spending on large-file windows.
    selected.sort(key=lambda row: (trees[row[2]][row[1]][1], row[1]))
    result["omitted_files"] = len({row[1] for row in changes + named + related}) - len(selected)
    for selected_index, (_priority, path, side) in enumerate(selected):
        oid, size = trees[side][path]
        if size > MAX_BLOB_BYTES:
            result["omitted_files"] += 1
            continue
        if side == "head" and path in sources:
            source = sources[path]
        else:
            raw = git(repository, "cat-file", "blob", oid)
            try:
                source = raw.decode("utf-8") if b"\0" not in raw else ""
            except UnicodeDecodeError:
                source = ""
        anchors = parsed["locations"].get((path, side), set()) - {0}
        anchors |= {i for i, line in enumerate(source.splitlines(), 1) if identifiers(line) & queries}
        budget = min(MAX_FILE_BYTES, (MAX_CONTEXT_BYTES - result["bytes"]) // (len(selected) - selected_index))
        text, ranges = excerpt(source, anchors, terms, budget, visible[(path, side)], relevance)
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
