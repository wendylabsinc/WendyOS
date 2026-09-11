#!/usr/bin/env python3
"""Render and publish a revision-bound API decision checklist."""

from __future__ import annotations

import argparse
import hashlib
import html
import json
import os
from pathlib import Path, PurePosixPath
import re
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

COMMENT_MARKER = "<!-- ai-api-review:v1 -->"
WARNING_START = "<!-- ai-api-review:incomplete -->"
WARNING_END = "<!-- /ai-api-review:incomplete -->"
MAX_COMMENT_BYTES = 65_000
WARNING_RESERVE_BYTES = 4_000
CATEGORIES = {
    "network": "Network contracts and constants",
    "protobuf": "Protobuf and RPC contracts",
    "storage": "Disk locations and persisted state",
    "config": "Configuration formats and environment",
    "cli": "CLI layout and behavior",
    "other": "Other durable contracts",
}
IMPACTS = {"additive": "Additive", "breaking": "Breaking", "behavioral": "Behavior change"}
API_LABEL = {
    "name": "api-review",
    "color": "B60205",
    "description": "Durable API decisions need review: network, protobuf, storage, config, or CLI",
}
RISK_LABELS = {
    "low": {"name": "risk: low", "color": "0E8A16", "description": "Low estimated risk; focused testing is sufficient"},
    "mid": {"name": "risk: mid", "color": "FBCA04", "description": "Medium estimated risk; test changes and adjacent workflows"},
    "high": {"name": "risk: high", "color": "B60205", "description": "High estimated risk; thoroughly test compatibility and affected workflows"},
}


def inline(value: str) -> str:
    """Model prose is plain text; only this renderer supplies Markdown/links."""
    value = html.escape(" ".join(value.split()), quote=False).replace("@", "&#64;")
    return re.sub(r"([\\`*_\[\]{}()#!|~])", r"\\\1", value)


def validate_result(result: dict, head_sha: str, base_sha: str) -> None:
    if not isinstance(result, dict) or result.get("status") not in {"complete", "incomplete"}:
        raise ValueError("API review result has an invalid status")
    for name, expected in (("head_sha", head_sha), ("base_sha", base_sha)):
        if not re.fullmatch(r"[0-9a-f]{40}", expected) or result.get(name) != expected:
            raise ValueError(f"API review result has a mismatched {name}")
    if result["status"] == "incomplete":
        if not isinstance(result.get("error"), str) or not result["error"].strip():
            raise ValueError("Incomplete API review must explain the failure")
        return
    if not re.fullmatch(r"[0-9a-f]{64}", result.get("diff_sha256", "")):
        raise ValueError("API review result has no diff fingerprint")
    if not re.fullmatch(r"[0-9a-f]{40}", result.get("diff_base_sha", "")):
        raise ValueError("API review result has no diff merge-base revision")
    for field in ("changed_files", "diff_bytes"):
        if type(result.get(field)) is not int or result[field] <= 0:
            raise ValueError(f"API review result has invalid {field}")
    if result.get("risk") not in RISK_LABELS or not isinstance(result.get("decisions"), list):
        raise ValueError("API review result has invalid decisions or testing risk")
    for decision in result["decisions"]:
        if not isinstance(decision, dict) or set(decision) != {
            "category", "title", "change", "compatibility", "impact", "locations",
        }:
            raise ValueError("API decision has unexpected fields")
        if decision["category"] not in CATEGORIES or decision["impact"] not in IMPACTS:
            raise ValueError("API decision has an invalid category or impact")
        for field in ("title", "change", "compatibility"):
            if not isinstance(decision[field], str) or not decision[field].strip():
                raise ValueError(f"API decision has invalid {field}")
        if not isinstance(decision["locations"], list) or not decision["locations"]:
            raise ValueError("API decision must link to code")
        for location in decision["locations"]:
            if not isinstance(location, dict) or set(location) != {"path", "side", "line", "end_line"}:
                raise ValueError("API location has unexpected fields")
            path = location["path"]
            if (not isinstance(path, str) or not path or path.startswith("/")
                    or "\\" in path or any(ord(char) < 32 for char in path)
                    or any(part in {".", ".."} for part in path.split("/"))
                    or str(PurePosixPath(path)) != path):
                raise ValueError("API location must be a repository-relative path")
            if (location["side"] not in {"base", "head"}
                    or type(location["line"]) is not int
                    or type(location["end_line"]) is not int
                    or not 0 <= location["line"] <= location["end_line"]
                    or (location["line"] == 0 and location["end_line"] != 0)):
                raise ValueError("API location has an invalid line range")


def revision_marker(result: dict) -> str:
    return (f"<!-- ai-api-review:head={result['head_sha']};base={result['base_sha']};"
            f"diff={result['diff_sha256']} -->")


def decision_id(decision: dict) -> str:
    return hashlib.sha256(json.dumps(decision, sort_keys=True).encode()).hexdigest()


def code_link(repo: str, result: dict, location: dict) -> str:
    sha = result["diff_base_sha"] if location["side"] == "base" else result["head_sha"]
    # Line zero is validated by the reviewer only for structural changes such
    # as a rename or file mode change, where Git provides no changed text lines.
    fragment = f"#L{location['line']}" if location["line"] else ""
    label = f"{location['path']}:{location['line']}" if location["line"] else location["path"]
    if location["end_line"] != location["line"]:
        fragment += f"-L{location['end_line']}"
        label += f"–{location['end_line']}"
    if location["side"] == "base":
        label += " (before)"
    url = f"https://github.com/{repo}/blob/{sha}/{urllib.parse.quote(location['path'], safe='/')}{fragment}"
    return f"[{inline(label)}]({url})"


def render_comment(result: dict, repo: str, previous: str = "") -> str:
    accepted = set()
    # Preserve only unchanged decisions for the exact reviewed diff. Model prose
    # changes conservatively require acceptance again, even on a rerun.
    if revision_marker(result) in previous:
        accepted = set(re.findall(
            r"^- \[[xX]\] .*<!-- api-decision:([0-9a-f]{64}) -->$", previous, re.MULTILINE,
        ))
    head = result["head_sha"]
    lines = [
        "# API decisions", "",
        f"Reviewed [{head[:12]}](https://github.com/{repo}/commit/{head}). "
        f"Input coverage: {result['changed_files']} changed files, {result['diff_bytes']:,} diff bytes "
        f"across {result.get('review_batches', 1)} complete batch(es); no truncation.",
        "",
        "Check a box to accept that decision for this revision. New commits reset acceptance. "
        "These checkboxes track API review and do not block merging automatically.",
        "",
        f"**Testing risk:** {result['risk']}. Compatibility impact is listed separately for each decision.",
        "",
    ]
    if not result["decisions"]:
        lines += ["No durable API decisions changed. Comments, formatting, and documentation wording alone do not require acceptance.", ""]
    for category, title in CATEGORIES.items():
        decisions = [item for item in result["decisions"] if item["category"] == category]
        lines += [f"## {title}", ""]
        if not decisions:
            lines += ["No API decisions changed.", ""]
            continue
        for decision in sorted(decisions, key=lambda item: (item["title"], decision_id(item))):
            identifier = decision_id(decision)
            checked = "x" if identifier in accepted else " "
            lines += [
                f"- [{checked}] Accept **{inline(decision['title'])}** — "
                f"{IMPACTS[decision['impact']]}. <!-- api-decision:{identifier} -->",
                f"  - Change: {inline(decision['change'])}",
                f"  - Compatibility: {inline(decision['compatibility'])}",
                "  - Code: " + ", ".join(code_link(repo, result, loc) for loc in decision["locations"]),
                "",
            ]
    lines += [revision_marker(result), COMMENT_MARKER, ""]
    body = "\n".join(lines)
    if len(body.encode()) > MAX_COMMENT_BYTES - WARNING_RESERVE_BYTES:
        raise ValueError("API decision checklist exceeds GitHub's comment limit; no decisions were truncated")
    return body


def render_incomplete(result: dict, repo: str, previous: str = "") -> str:
    previous = re.sub(
        re.escape(WARNING_START) + r".*?" + re.escape(WARNING_END), "", previous, flags=re.DOTALL,
    ).replace(COMMENT_MARKER, "").strip()
    if not previous:
        previous = "# API decisions"
    head = result["head_sha"]
    warning = "\n".join([
        WARNING_START,
        "> [!WARNING]",
        f"> API review is incomplete for [{head[:12]}](https://github.com/{repo}/commit/{head}): {inline(result['error'][:400])}",
        "> Any checklist below belongs to the previous successful review. Prior decisions, acceptance, and risk labels are preserved; no clean result was recorded.",
        WARNING_END,
    ])
    lines = previous.splitlines()
    lines[1:1] = ["", warning, ""]
    body = "\n".join(lines).strip() + f"\n\n{COMMENT_MARKER}\n"
    if len(body.encode()) > MAX_COMMENT_BYTES:
        # Never cut off an earlier checklist or its human acceptance.
        raise ValueError("Cannot append failure notice without truncating the previous checklist")
    return body


class GitHub:
    def __init__(self, token: str):
        self.token = token

    def request(self, method: str, path: str, payload=None):
        headers = {
            "Accept": "application/vnd.github+json",
            "Authorization": f"Bearer {self.token}",
            "X-GitHub-Api-Version": "2022-11-28",
            "User-Agent": "wendy-api-review",
        }
        data = None
        if payload is not None:
            data = json.dumps(payload).encode()
            headers["Content-Type"] = "application/json"
        request = urllib.request.Request("https://api.github.com" + path, data=data, headers=headers, method=method)
        for attempt in range(3):
            try:
                with urllib.request.urlopen(request, timeout=30) as response:
                    body = response.read()
                    return json.loads(body) if body else None
            except urllib.error.HTTPError as error:
                if method == "POST" or error.code not in {429, 500, 502, 503, 504} or attempt == 2:
                    raise
            except urllib.error.URLError:
                # A lost response does not mean creation failed. A rerun will
                # discover the bot-owned comment; blind POST retries duplicate it.
                if method == "POST" or attempt == 2:
                    raise
            time.sleep(2**attempt)


def publish(result: dict, repo: str, pr_number: int, head_sha: str, base_sha: str, github) -> bool:
    if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repo) or pr_number <= 0:
        raise ValueError("Invalid repository or PR number")
    validate_result(result, head_sha, base_sha)
    root = f"/repos/{repo}"
    issue = f"{root}/issues/{pr_number}"
    pull_path = f"{root}/pulls/{pr_number}"

    def current_pull():
        pull = github.request("GET", pull_path)
        if (pull["head"]["sha"] != head_sha or pull["base"]["sha"] != base_sha
                or pull["head"]["repo"]["full_name"].lower() != repo.lower()
                or pull["state"] != "open"):
            raise ValueError("PR revision or state changed during API review; refusing stale publication")
        return pull

    current_pull()
    existing = []
    for page in range(1, 101):
        comments = github.request("GET", f"{issue}/comments?per_page=100&page={page}")
        existing.extend(comment for comment in comments if (
            (comment.get("user") or {}).get("login") == "github-actions[bot]"
            and (comment.get("user") or {}).get("type") == "Bot"
            and COMMENT_MARKER in (comment.get("body") or "")
        ))
        if len(comments) < 100:
            break
    else:
        raise ValueError("Could not enumerate all previous API review comments")
    existing.sort(key=lambda comment: comment["id"])
    previous = existing[0]["body"] if existing else ""
    complete = result["status"] == "complete"
    try:
        body = render_comment(result, repo, previous) if complete else render_incomplete(result, repo, previous)
    except ValueError as error:
        if not complete:
            raise
        result = {**result, "status": "incomplete", "error": str(error)}
        complete = False
        body = render_incomplete(result, repo, previous)
    # A model call can take minutes. Recheck after reading state and directly
    # before mutations so a superseded run cannot overwrite a newer checklist.
    pull = current_pull()
    if existing:
        github.request("PATCH", f"{root}/issues/comments/{existing[0]['id']}", {"body": body})
    else:
        github.request("POST", f"{issue}/comments", {"body": body})

    def ensure_label(label):
        path = f"{root}/labels/{urllib.parse.quote(label['name'], safe='')}"
        try:
            current = github.request("GET", path)
        except urllib.error.HTTPError as error:
            if error.code != 404:
                raise
            github.request("POST", f"{root}/labels", label)
        else:
            if any(current.get(key) != value for key, value in label.items() if key != "name"):
                github.request("PATCH", path, {"color": label["color"], "description": label["description"]})

    def remove_label(name):
        try:
            github.request("DELETE", f"{issue}/labels/{urllib.parse.quote(name, safe='')}")
        except urllib.error.HTTPError as error:
            if error.code != 404:
                raise

    if not complete:
        # An unavailable review is never reported as no API change. Retain all
        # risk labels and request manual attention, including on the first run.
        ensure_label(API_LABEL)
        github.request("POST", f"{issue}/labels", {"labels": [API_LABEL["name"]]})
        return False

    risk = RISK_LABELS[result["risk"]]
    ensure_label(risk)
    for level, label in RISK_LABELS.items():
        if level != result["risk"]:
            remove_label(label["name"])
    labels = [risk["name"]]
    if result["decisions"]:
        ensure_label(API_LABEL)
        labels.append(API_LABEL["name"])
    else:
        remove_label(API_LABEL["name"])
    github.request("POST", f"{issue}/labels", {"labels": labels})
    reviewer = "joannis"
    if (result["decisions"] and pull["user"]["login"].lower() != reviewer
            and not any(user["login"].lower() == reviewer for user in pull["requested_reviewers"])):
        github.request("POST", f"{pull_path}/requested_reviewers", {"reviewers": [reviewer]})
    return True


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=["publish"])
    for option in ("result", "repo", "expected-head-sha", "expected-base-sha"):
        parser.add_argument("--" + option, required=True)
    parser.add_argument("--pr-number", required=True, type=int)
    args = parser.parse_args()
    token = os.environ.get("GH_TOKEN")
    if not token:
        raise ValueError("GH_TOKEN is required to publish API review")
    result = json.loads(Path(args.result).read_text())
    return 0 if publish(result, args.repo, args.pr_number, args.expected_head_sha,
                        args.expected_base_sha, GitHub(token)) else 1


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (ValueError, OSError, urllib.error.URLError) as error:
        print(f"API review publication failed: {error}", file=sys.stderr)
        sys.exit(1)
