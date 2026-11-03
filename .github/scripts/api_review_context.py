"""Bounded immutable source context for API Review, never executable PR code."""
from __future__ import annotations

import pathlib
import subprocess

from review_sources import is_generated_path, valid_path

MAX_CONTEXT_BYTES = 48_000
MAX_FILE_BYTES = 8_000
MAX_BLOB_BYTES = 1_000_000
MAX_FILES = 12
SOURCE_SUFFIXES = {".go", ".swift", ".proto", ".py", ".sh", ".json", ".yaml", ".yml", ".toml"}
LIMITATION = (
    "Partial context: changed files (large files excerpted around their first change) and nearby unchanged source only, from the diff merge base and head. "
    "Not a caller graph or release/deployment inventory. External schemas are not fetched. "
    "Missing callers, versioned schemas, fallbacks or retirement evidence mean insufficient evidence, not compatibility."
)


def git(repository: str, *args: str) -> bytes:
    return subprocess.run(["git", "-C", repository, *args], check=True, capture_output=True,
                          timeout=30).stdout


def source_context(repository: str, parsed: dict, base: str, head: str) -> dict:
    """Read bounded blobs after canonical generated-path and file-mode checks.

    Keep selection deliberately small: changed source first, then siblings.
    Context supports reasoning, but can never supply changed-line evidence IDs.
    """
    changed = {path for path, _side in parsed["locations"]}
    parents = {str(pathlib.PurePosixPath(path).parent) for path in changed}
    result = {"limitation": LIMITATION, "files": [], "omitted_files": 0, "bytes": 0, "partial_files": 0}
    candidates = []
    for side, revision in (("base", base), ("head", head)):
        tree = git(repository, "ls-tree", "-rlz", revision)
        for row in tree.split(b"\0"):
            if not row:
                continue
            fields, name = row.split(b"\t", 1)
            mode, kind, oid, size = fields.decode("ascii").split()
            path = name.decode("utf-8", errors="replace")
            # Never read generated blobs, symlinks, submodules or unsafe paths.
            if (mode not in {"100644", "100755"} or kind != "blob" or not valid_path(path)
                    or is_generated_path(path) or pathlib.PurePosixPath(path).suffix not in SOURCE_SUFFIXES):
                continue
            if path in changed or str(pathlib.PurePosixPath(path).parent) in parents:
                candidates.append((path not in changed, path, side, revision, oid, int(size)))

    for unchanged, path, side, revision, oid, size in sorted(candidates):
        if (len(result["files"]) >= MAX_FILES or size > MAX_BLOB_BYTES
                or (unchanged and size > MAX_FILE_BYTES)
                or result["bytes"] + min(size, MAX_FILE_BYTES) > MAX_CONTEXT_BYTES):
            result["omitted_files"] += 1
            continue
        raw = git(repository, "cat-file", "blob", oid)
        try:
            source = raw.decode("utf-8")
        except UnicodeDecodeError:
            result["omitted_files"] += 1
            continue
        if "\0" in source:
            result["omitted_files"] += 1
            continue
        start_line = 1
        partial = size > MAX_FILE_BYTES
        if partial:
            anchor = min((line for line in parsed["locations"].get((path, side), {1}) if line), default=1)
            start_line = max(1, anchor - 30)
            source = "".join(source.splitlines(keepends=True)[start_line - 1:anchor + 30])
            source = source.encode("utf-8")[:MAX_FILE_BYTES].decode("utf-8", errors="ignore")
        result["partial_files"] += int(partial)
        result["files"].append({"path": path, "side": side, "sha": revision,
                                "unchanged": unchanged, "partial": partial,
                                "start_line": start_line, "source": source})
        result["bytes"] += len(source.encode("utf-8"))
    return result
