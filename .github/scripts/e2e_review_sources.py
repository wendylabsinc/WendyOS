#!/usr/bin/env python3
"""Prepare E2E review inputs with the canonical policy from a trusted Git base."""
import argparse
import importlib
import json
from pathlib import Path
import subprocess
import sys
import tempfile


ARTIFACTS = ("git-diff-source.diff", "git-diff-source-input.json",
             "git-diff-name-only.txt", "git-diff-stat.txt", "review-source-files.json")
TOOL_READ_LIMITATION = (
    "Source-only filtering applies to these prepared inputs, not to provider or custom-harness "
    "tool responses. Their repository/file/shell tools remain unrestricted. Do not retrieve or "
    "review excluded generated implementation through those tools. This is review-scope guidance, "
    "not a filesystem sandbox or an enforced tool-read boundary."
)


def git(repo, *arguments):
    return subprocess.check_output(["git", "-C", str(repo), *arguments], stderr=subprocess.PIPE)


def commit(repo, ref):
    return git(repo, "rev-parse", "--verify", "--end-of-options", ref + "^{commit}").decode().strip()


def prepare(repo, run, policy_ref, diff=None):
    for name in ARTIFACTS:
        (run / name).unlink(missing_ok=True)
    policy_sha = commit(repo, policy_ref)
    head = commit(repo, "HEAD")
    # Neither the event-head module nor PYTHONPATH supplies review policy.
    # Both siblings come from this one immutable trusted automation/base commit.
    with tempfile.TemporaryDirectory(prefix="wendy-e2e-review-policy-") as directory:
        for name in ("review_sources.py", "review_diff.py"):
            Path(directory, name).write_bytes(git(repo, "show", f"{policy_sha}:.github/scripts/{name}"))
        sys.path.insert(0, directory)
        try:
            policy = importlib.import_module("review_sources")
            tree = []
            for record in git(repo, "ls-tree", "-rz", "--full-tree", head).split(b"\0"):
                if not record:
                    continue
                properties, path = record.split(b"\t", 1)
                mode, kind, oid = properties.decode().split()
                path = path.decode("utf-8")
                if not policy.valid_path(path):
                    raise policy.ReviewError("The committed source tree contains an invalid review path")
                if mode == "100644" and policy.is_generated_path(path):
                    continue
                tree.append({"path": path, "mode": mode, "kind": kind, "oid": oid})

            source, coverage, index, base = "", None, [], None
            status = "full_source_index"
            if diff is not None:
                separator = "..." if "..." in diff else ".." if ".." in diff else None
                revisions = diff.split(separator) if separator else [diff, "HEAD"]
                if len(revisions) != 2 or not all(revisions):
                    raise policy.ReviewError("Expected a complete BASE...HEAD or BASE..HEAD review range")
                base, range_head = (commit(repo, value) for value in revisions)
                # The tree index and patch must describe the same committed head.
                if range_head != head:
                    raise policy.ReviewError("The review range must end at the checked-out HEAD commit")
                if separator == "...":
                    base = git(repo, "merge-base", base, head).decode().strip()
                raw = git(repo, "-c", "core.quotePath=true", "diff", "--no-ext-diff",
                          "--no-textconv", "--no-color", "--no-relative", "--src-prefix=a/",
                          "--dst-prefix=b/", "--find-renames", base, head, "--").decode("utf-8")
                # Empty Git output means no committed changes; malformed nonempty
                # input still goes through the complete canonical parser and fails.
                if raw:
                    source, coverage = policy.source_diff(raw, allow_binary=True)
                status = "source_diff" if source else "generated_only" if raw else "no_changes"
                files = policy.parse_diff(source, allow_binary=True)["files"] if source else []
                index = [{"base": f["base"], "head": f["head"],
                          "additions": len(f["head_lines"]), "deletions": len(f["base_lines"]),
                          "structural": f["structural"]} for f in files]

            manifest = {"status": status, "policy_commit": policy_sha, "head_commit": head,
                        "diff_base_commit": base, "coverage": coverage, "index": index,
                        "policy_rule": policy.SOURCE_ONLY_RULE,
                        "tool_read_limitation": TOOL_READ_LIMITATION,
                        "source_snapshot": "Committed files only; uncommitted/untracked source is not prepared.",
                        "evidence_scope": "Prepared input counts do not prove model review. Binary placeholders do not supply binary contents. Retain failed-run, test, qualitative AI-request and prior-finding evidence even when the diff is generated-only."}
            names = sorted({f[side] for f in index for side in ("base", "head") if f[side]})
            stats = [f"{f['base'] or f['head']}" +
                     (f" => {f['head']}" if f['base'] and f['head'] and f['base'] != f['head'] else "") +
                     f" | +{f['additions']} -{f['deletions']}" +
                     (" (structural)" if f['structural'] else "") for f in index]
            (run / ARTIFACTS[0]).write_bytes(source.encode("utf-8"))
            (run / ARTIFACTS[2]).write_text("\n".join(names) + ("\n" if names else ""), encoding="utf-8")
            (run / ARTIFACTS[3]).write_text("\n".join(stats) + ("\n" if stats else ""), encoding="utf-8")
            (run / ARTIFACTS[4]).write_text(json.dumps(tree, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
            (run / ARTIFACTS[1]).write_text(json.dumps(manifest, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
        finally:
            sys.path.remove(directory)
            for name in ("review_sources", "review_diff"):
                sys.modules.pop(name, None)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repo", required=True, type=Path)
    parser.add_argument("--run-dir", required=True, type=Path)
    parser.add_argument("--policy-ref", required=True)
    parser.add_argument("--diff")
    args = parser.parse_args()
    try:
        prepare(args.repo, args.run_dir, args.policy_ref, args.diff)
    except (OSError, ValueError, RuntimeError, ImportError, subprocess.CalledProcessError) as error:
        # No raw patch is written to disk or included in an error response.
        message = "Source-only E2E input preparation failed: " + str(error)
        (args.run_dir / ARTIFACTS[1]).write_text(json.dumps({"status": "error", "error": message}) + "\n")
        print(message, file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
