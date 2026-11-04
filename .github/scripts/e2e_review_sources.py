#!/usr/bin/env python3
"""Prepare E2E review inputs with the canonical policy from a trusted Git base."""
import argparse
import importlib
import json
from pathlib import Path
import re
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


class GitError(RuntimeError):
    """A failed Git command, carrying Git's own reason."""


def git(repo, *arguments):
    try:
        return subprocess.check_output(["git", "-C", str(repo), *arguments], stderr=subprocess.PIPE)
    except subprocess.CalledProcessError as error:
        # str(CalledProcessError) omits stderr, which holds the actual reason.
        reason = error.stderr.decode("utf-8", errors="replace").strip() or f"exit status {error.returncode}"
        raise GitError(f"git {' '.join(arguments)}: {reason}") from error


def commit(repo, ref):
    return git(repo, "rev-parse", "--verify", "--end-of-options", ref + "^{commit}").decode().strip()


def non_utf8_files(policy, raw):
    """Name the files whose diff lost bytes to U+FFFD when decoded."""
    files = []
    for section in re.split(rb"^(?=diff --git )", raw, flags=re.M):
        try:
            section.decode("utf-8")
        except UnicodeDecodeError:
            parsed = policy.parse_diff(section.decode("utf-8", errors="replace"), allow_binary=True)
            files += [file["head"] or file["base"] for file in parsed["files"]]
    return files


def prepare(repo, run, policy_ref, diff=None):
    for name in ARTIFACTS:
        (run / name).unlink(missing_ok=True)
    try:
        policy_sha = commit(repo, policy_ref)
    except GitError as error:
        raise GitError(f"Could not resolve source policy ref {policy_ref}. "
                       f"Pass a fetched commit with --source-policy-ref. {error}") from error
    head = commit(repo, "HEAD")
    # Both policy modules come from this one commit, not the working tree or
    # PYTHONPATH. PR events pin it to the base, but this adapter runs from the
    # event head, and on push and dispatch the policy ref is the event head too.
    with tempfile.TemporaryDirectory(prefix="wendy-e2e-review-policy-") as directory:
        for name in ("review_sources.py", "review_diff.py"):
            try:
                module = git(repo, "show", f"{policy_sha}:.github/scripts/{name}")
            except GitError as error:
                raise GitError(f"Source policy file .github/scripts/{name} is missing at {policy_sha} "
                               f"(from {policy_ref}). Pass a commit that has both policy modules with "
                               f"--source-policy-ref. {error}") from error
            Path(directory, name).write_bytes(module)
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
                try:
                    base, range_head = (commit(repo, value) for value in revisions)
                    # The tree index and patch must describe the same committed head.
                    if range_head != head:
                        raise policy.ReviewError("The review range must end at the checked-out HEAD commit")
                    if separator == "...":
                        base = git(repo, "merge-base", base, head).decode().strip()
                except GitError as error:
                    raise GitError(f"Could not resolve review range {diff}. Ensure the repository has "
                                   f"enough history and the range is fetchable. {error}") from error
                raw = git(repo, "-c", "core.quotePath=true", "diff", "--no-ext-diff",
                          "--no-textconv", "--no-color", "--no-relative", "--src-prefix=a/",
                          "--dst-prefix=b/", "--find-renames", base, head, "--")
                # Empty Git output means no committed changes; malformed nonempty
                # input still goes through the complete canonical parser and fails.
                if raw:
                    # Git emits legacy-encoded text as raw bytes. One such file must
                    # not fail the range, so coverage records what was replaced.
                    source, coverage = policy.source_diff(raw.decode("utf-8", errors="replace"), allow_binary=True)
                    coverage["non_utf8_files"] = non_utf8_files(policy, raw)
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
                        "evidence_scope": "Prepared input counts do not prove model review. Binary placeholders do not supply binary contents. Files in coverage.non_utf8_files show U+FFFD where their bytes were not valid UTF-8. Retain failed-run, test and qualitative AI-request evidence even when the diff is generated-only. Findings from earlier CI runs are not supplied here."}
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
    except (OSError, ValueError, RuntimeError, ImportError) as error:
        # No raw patch is written to disk or included in an error response.
        message = "Source-only E2E input preparation failed: " + str(error)
        (args.run_dir / ARTIFACTS[1]).write_text(json.dumps({"status": "error", "error": message}) + "\n")
        print(message, file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
