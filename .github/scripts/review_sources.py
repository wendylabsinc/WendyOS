"""Fixed generator-output policy and complete Git patch parsing for AI reviews.

Exclusion is a path and suffix policy over the locations the repository's protocol
generators write to. It does not confirm a file came from a generator, and most
excluded outputs have no regeneration check. PR attributes, headers, vendor roots
and lockfiles never define review policy.
"""
from __future__ import annotations

import hashlib
import re
from typing import Any

from review_diff import diff_lines

HUNK_RE = re.compile(r"^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@(?:.*)$")
SOURCE_ONLY_RULE = (
    "Review source material only. Do not review generated code or request edits to it. "
    "Generated protocol outputs are excluded before model input. Review .proto definitions, "
    "generators, version pins, configuration and generation/build checks when supplied. "
    "Do not infer missing behavior or resolved prior findings from excluded output. "
    "Preserve and assess changed external schema/source pins and source references. "
    "If upstream source is not supplied, state that inspection limitation honestly. "
    "Exclusion and passing generation checks do not establish API or behavior coverage."
)


def is_generated_path(path: str) -> bool:
    if not valid_path(path):
        return False
    # go/scripts/generate-proto.sh writes protoc Go output here.
    if path.startswith("go/proto/gen/") and path.endswith(".pb.go"):
        return True
    # swift/Scripts/GenerateProto.sh has exactly these three output roots.
    roots = tuple("swift/WendyAgentCore/Sources/" + module + "/Proto/" for module in
                  ("WendyAgentGRPC", "WendyCloudGRPC", "OpenTelemetryGRPC"))
    if path.startswith(roots) and path.endswith((".pb.swift", ".grpc.swift")):
        return True
    # The example regenerate-grpc.sh scripts use PathToUnderscores.
    return path in {
        "Examples/WendyNotify/Sources/WendyNotify/Generated/wendy_system_v1_notifications.pb.swift",
        "Examples/WendyNotify/Sources/WendyNotify/Generated/wendy_system_v1_notifications.grpc.swift",
        "Examples/ThermalNotify/Sources/ThermalNotify/Generated/wendy_system_v1_notifications.pb.swift",
        "Examples/ThermalNotify/Sources/ThermalNotify/Generated/wendy_system_v1_notifications.grpc.swift",
        "Examples/ThermalNotify/Sources/ThermalNotify/Generated/wendy_agent_services_v1_wendy_agent_v1_video_service.pb.swift",
        "Examples/ThermalNotify/Sources/ThermalNotify/Generated/wendy_agent_services_v1_wendy_agent_v1_video_service.grpc.swift",
    }


class ReviewError(RuntimeError):
    """A safe, human-readable explanation of an incomplete review."""


def valid_path(path: Any) -> bool:
    return (
        isinstance(path, str)
        and bool(path)
        and len(path) <= 2000
        and not path.startswith("/")
        and "\\" not in path
        and not any(part in {"", ".", ".."} for part in path.split("/"))
        and not any(ord(character) < 32 or ord(character) == 127 for character in path)
    )


def git_path(value: str) -> str:
    """Decode Git's C-quoted UTF-8 filenames, including octal byte escapes."""
    if not value.startswith('"'):
        return value
    if not value.endswith('"'):
        raise ReviewError("The diff contains an invalid quoted path")
    result = bytearray()
    value = value[1:-1]
    position = 0
    escapes = {"a": 7, "b": 8, "t": 9, "n": 10, "v": 11, "f": 12, "r": 13, '"': 34, "\\": 92}
    while position < len(value):
        character = value[position]
        position += 1
        if character != "\\":
            result.extend(character.encode("utf-8"))
            continue
        match = re.match(r"[0-7]{1,3}", value[position:])
        if match:
            byte = int(match.group(), 8)
            if byte > 255:
                raise ReviewError("The diff contains an invalid path escape")
            result.append(byte)
            position += len(match.group())
        elif position < len(value) and value[position] in escapes:
            result.append(escapes[value[position]])
            position += 1
        else:
            raise ReviewError("The diff contains an invalid path escape")
    try:
        return result.decode("utf-8")
    except UnicodeDecodeError as error:
        raise ReviewError("The diff contains a non-UTF-8 path") from error


def _header_paths(header: str) -> tuple[str, str]:
    # Git leaves spaces unquoted. For unchanged names, match the repeated name
    # before considering the general old/new header used by renames.
    if header.startswith("a/"):
        for match in re.finditer(r" b/", header):
            old, new = header[2:match.start()], header[match.end():]
            if old == new:
                return old, new
    tokens = re.fullmatch(r'("(?:[^"\\]|\\.)*"|a/.*) ("(?:[^"\\]|\\.)*"|b/.*)', header)
    if not tokens:
        raise ReviewError("The diff contains an invalid file header")
    old, new = (git_path(token) for token in tokens.groups())
    if not old.startswith("a/") or not new.startswith("b/"):
        raise ReviewError("The diff contains an invalid file prefix")
    return old[2:], new[2:]


def parse_diff(diff: str, *, allow_binary: bool = False) -> dict[str, Any]:
    """Track every file and every changed line on its correct revision side."""
    files: list[dict[str, Any]] = []
    current: dict[str, Any] | None = None
    remaining_base = remaining_head = 0
    base_line = head_line = 0
    in_hunk = False
    additions = deletions = 0
    line_evidence: list[tuple[int, dict[str, Any], str, int]] = []

    def finish_hunk() -> None:
        if remaining_base or remaining_head:
            raise ReviewError("The PR diff has an incomplete or malformed hunk")

    for patch_line, line in enumerate(diff_lines(diff)):
        if line.startswith("diff --git "):
            finish_hunk()
            old, new = _header_paths(line[len("diff --git "):])
            current = {"header_base": old, "header_head": new, "base": old, "head": new, "base_lines": set(), "head_lines": set(), "structural": False, "patch_line": patch_line}
            files.append(current)
            in_hunk = False
            continue
        if current is None:
            if line.strip():
                raise ReviewError("The PR diff has content outside a file patch")
            continue
        match = HUNK_RE.fullmatch(line)
        if match:
            finish_hunk()
            base_line, head_line = int(match[1]), int(match[3])
            remaining_base = int(match[2]) if match[2] is not None else 1
            remaining_head = int(match[4]) if match[4] is not None else 1
            if (remaining_base and base_line < 1) or (remaining_head and head_line < 1):
                raise ReviewError("The PR diff has an invalid hunk line number")
            in_hunk = True
            continue
        if in_hunk:
            if line == "\\ No newline at end of file":
                continue
            if line.startswith("+") and remaining_head:
                line_evidence.append((patch_line, current, "head", head_line))
                current["head_lines"].add(head_line)
                additions += 1
                head_line += 1
                remaining_head -= 1
            elif line.startswith("-") and remaining_base:
                line_evidence.append((patch_line, current, "base", base_line))
                current["base_lines"].add(base_line)
                deletions += 1
                base_line += 1
                remaining_base -= 1
            elif line.startswith(" ") and remaining_base and remaining_head:
                base_line += 1
                head_line += 1
                remaining_base -= 1
                remaining_head -= 1
            else:
                raise ReviewError("The PR diff has an incomplete or malformed hunk")
        elif line.startswith(("--- ", "+++ ")):
            side = "base" if line.startswith("--- ") else "head"
            # Git appends one TAB to a path containing a space, outside any
            # quotes. A real tab in a name is always C-quoted.
            path = git_path(line[4:].removesuffix("\t"))
            prefix = "a/" if side == "base" else "b/"
            if path == "/dev/null":
                current[side] = None
            elif path.startswith(prefix):
                if path[2:] != current["header_" + side]:
                    raise ReviewError("The diff patch path disagrees with its file header")
                current[side] = path[2:]
            else:
                raise ReviewError("The diff contains an invalid patch path")
        elif line.startswith("rename from "):
            current["base"] = git_path(line[len("rename from "):])
            if current["base"] != current["header_base"]:
                raise ReviewError("The diff rename/copy path disagrees with its file header")
            current["structural"] = True
        elif line.startswith("rename to "):
            current["head"] = git_path(line[len("rename to "):])
            if current["head"] != current["header_head"]:
                raise ReviewError("The diff rename/copy path disagrees with its file header")
            current["structural"] = True
        elif line.startswith("copy from "):
            current["base"] = git_path(line[len("copy from "):])
            if current["base"] != current["header_base"]:
                raise ReviewError("The diff rename/copy path disagrees with its file header")
            current["structural"] = True
        elif line.startswith("copy to "):
            current["head"] = git_path(line[len("copy to "):])
            if current["head"] != current["header_head"]:
                raise ReviewError("The diff rename/copy path disagrees with its file header")
            current["structural"] = True
        elif line.startswith(("new file mode ", "deleted file mode ", "old mode ", "new mode ")):
            current["structural"] = True
            if line.startswith("new file mode "):
                current["base"] = None
            elif line.startswith("deleted file mode "):
                current["head"] = None
        elif line.startswith(("Binary files ", "GIT binary patch")):
            if not allow_binary:
                raise ReviewError("The PR includes a binary patch that cannot be completely reviewed as text")
        elif not line.startswith(("index ", "new file mode ", "deleted file mode ", "old mode ", "new mode ", "similarity index ", "dissimilarity index ")) and line.strip():
            raise ReviewError("The PR diff contains an unsupported patch record")
    finish_hunk()
    if not files:
        raise ReviewError("The PR diff is empty or contains no file patches")
    locations: dict[tuple[str, str], set[int]] = {}
    structural_evidence: dict[int, list[dict[str, Any]]] = {}
    seen: set[tuple[str | None, str | None]] = set()
    for file in files:
        pair = (file["base"], file["head"])
        if pair in seen:
            raise ReviewError("The PR diff contains duplicate file patches")
        seen.add(pair)
        for side in ("base", "head"):
            path = file[side]
            if path is None:
                if file[f"{side}_lines"]:
                    raise ReviewError("The PR diff changes lines on a nonexistent file side")
                continue
            if not valid_path(path):
                raise ReviewError("The PR diff contains an invalid repository path")
            locations.setdefault((path, side), set()).update(file[f"{side}_lines"])
            if file["structural"]:
                # Zero denotes a file-level link for a changed name or mode;
                # metadata-only patches have no source line to anchor to.
                locations[(path, side)].add(0)
                structural_evidence.setdefault(file["patch_line"], []).append(
                    {"path": path, "side": side, "line": 0, "end_line": 0})
    return {
        "files": files, "changed_files": len(files), "additions": additions, "deletions": deletions, "locations": locations,
        "structural_evidence": structural_evidence,
        "line_evidence": {index: {"path": file[side], "side": side, "line": line, "end_line": line}
                          for index, file, side, line in line_evidence},
    }



def source_diff(diff: str, *, allow_binary: bool = False) -> tuple[str, dict[str, Any]]:
    """Validate ALL patches before excluding regular generated files on both sides.

    Renames across the source/output boundary and mode changes remain reviewable.
    The kept input is an exact concatenation of original patches, never rewritten.
    """
    parsed = parse_diff(diff, allow_binary=allow_binary)
    lines = diff_lines(diff, keepends=True)
    kept, excluded = [], []
    files = parsed["files"]
    for index, file in enumerate(files):
        end = files[index + 1]["patch_line"] if index + 1 < len(files) else len(lines)
        start = file["patch_line"] if index else 0
        section = "".join(lines[start:end])
        paths = [file[side] for side in ("base", "head") if file[side] is not None]
        # Permission, symlink and submodule changes are meaningful source changes.
        modes = re.findall(r"^(?:old mode|new mode|new file mode|deleted file mode) (\d+)$", section, re.M)
        modes += re.findall(r"^index [0-9a-f]+\.\.[0-9a-f]+ (\d+)$", section, re.M)
        regular = all(mode == "100644" for mode in modes)
        if paths and regular and all(is_generated_path(path) for path in paths):
            excluded.append({"path": file["head"] or file["base"], "diff_bytes": len(section.encode())})
        else:
            kept.append(section)
    source = "".join(kept)
    return source, {
        "diff_sha256": hashlib.sha256(diff.encode()).hexdigest(),
        "diff_bytes": len(diff.encode()), "changed_files": parsed["changed_files"],
        "source_bytes": len(source.encode()), "source_files": parsed["changed_files"] - len(excluded),
        "excluded_generated_files": excluded,
        "excluded_generated_bytes": sum(item["diff_bytes"] for item in excluded),
        "source_sha256": hashlib.sha256(source.encode()).hexdigest(),
    }
