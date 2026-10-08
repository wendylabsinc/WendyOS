#!/usr/bin/env python3
"""Render and publish a stateful API decision checklist."""

from __future__ import annotations

import argparse
import base64
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
import zlib

from api_review import illustrative_excerpt, prior_state_digest, validate_prior_state

COMMENT_MARKER = "<!-- ai-api-review:v1 -->"
PART_MARKER_RE = re.compile(r"<!-- ai-api-review:part=(\d+)/(\d+) -->")
STATE_MARKER_RE = re.compile(r"<!-- api-decision:v2:([A-Za-z0-9_-]+) -->")
LEGACY_DECISION_RE = re.compile(r"<!-- api-decision:([0-9a-f]{64}) -->")
STATE_VERSION = 2
DECISION_FIELDS = ("category", "title", "change", "compatibility", "impact", "locations")
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
IMPACTS = {
    "additive": ("🟢", "Additive"),
    "breaking": ("🔴", "Breaking"),
    "behavioral": ("🟡", "Behavior change"),
}
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
    """Render only balanced model-supplied inline-code spans as Markdown."""
    value = " ".join(value.split())

    def plain(text: str) -> str:
        text = html.escape(text, quote=False).replace("@", "&#64;")
        return re.sub(r"([\\`*_\[\]{}()#!|~])", r"\\\1", text)

    rendered: list[str] = []
    position = 0
    for match in re.finditer(r"`([^`]+)`", value):
        rendered.append(plain(value[position:match.start()]))
        code = html.escape(match.group(1), quote=False).replace("@", "&#64;")
        rendered.append(f"`{code}`")
        position = match.end()
    rendered.append(plain(value[position:]))
    return "".join(rendered)


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
    if not re.fullmatch(r"[0-9a-f]{64}", result.get("prior_state_sha256", "")):
        raise ValueError("API review result has no prior-state fingerprint")
    if not re.fullmatch(r"[0-9a-f]{40}", result.get("diff_base_sha", "")):
        raise ValueError("API review result has no diff merge-base revision")
    for field in ("changed_files", "diff_bytes"):
        if type(result.get(field)) is not int or result[field] <= 0:
            raise ValueError(f"API review result has invalid {field}")
    if result.get("risk") not in RISK_LABELS or not isinstance(result.get("decisions"), list):
        raise ValueError("API review result has invalid decisions or testing risk")
    matched_prior_ids: set[str] = set()
    for decision in result["decisions"]:
        if not isinstance(decision, dict) or set(decision) != {
            *DECISION_FIELDS, "prior_ids", "relationship", "reason",
        }:
            raise ValueError("API decision has unexpected fields")
        if decision["category"] not in CATEGORIES or decision["impact"] not in IMPACTS:
            raise ValueError("API decision has an invalid category or impact")
        relationship, candidates, reason = (decision["relationship"], decision["prior_ids"], decision["reason"])
        if (relationship not in {"new", "unchanged", "changed", "ambiguous"}
                or not isinstance(candidates, list) or len(candidates) > 8
                or len(set(candidates)) != len(candidates)
                or any(not isinstance(item, str) or not re.fullmatch(r"[0-9a-f]{64}", item) for item in candidates)
                or any(item in matched_prior_ids for item in candidates)
                or not isinstance(reason, str) or len(reason) > 500):
            raise ValueError("API decision has invalid reconciliation state")
        matched_prior_ids.update(candidates)
        valid_reconciliation = (
            (relationship == "new" and not candidates and not reason)
            or (relationship == "unchanged" and len(candidates) == 1 and not reason)
            or (relationship == "changed" and len(candidates) == 1 and bool(reason.strip()))
            or (relationship == "ambiguous" and bool(candidates) and bool(reason.strip()))
        )
        if not valid_reconciliation:
            raise ValueError("API decision reconciliation does not match its candidates")
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


def semantic_decision(decision: dict) -> dict:
    return {field: decision[field] for field in DECISION_FIELDS}


def decision_id(decision: dict) -> str:
    return hashlib.sha256(json.dumps(semantic_decision(decision), sort_keys=True).encode()).hexdigest()


def state_marker(record: dict) -> str:
    payload = json.dumps(record, sort_keys=True, separators=(",", ":")).encode()
    encoded = base64.urlsafe_b64encode(zlib.compress(payload, level=9)).decode().rstrip("=")
    return f"<!-- api-decision:v2:{encoded} -->"


def decode_state_marker(encoded: str) -> dict:
    if len(encoded) > 20_000:
        raise ValueError("API decision state marker is too large")
    try:
        compressed = base64.urlsafe_b64decode(encoded + "=" * (-len(encoded) % 4))
        decompressor = zlib.decompressobj()
        raw = decompressor.decompress(compressed, 12_001)
        if decompressor.unconsumed_tail or decompressor.unused_data or not decompressor.eof or len(raw) > 12_000:
            raise ValueError("API decision state marker expands beyond its limit")
        value = json.loads(raw)
    except (ValueError, TypeError, zlib.error) as error:
        raise ValueError("API decision state marker is invalid") from error
    expected = {"id", "version", "accepted_version", "rendered_checked", "state", "decision"}
    if not isinstance(value, dict) or set(value) != expected:
        raise ValueError("API decision state marker has unexpected fields")
    if (not re.fullmatch(r"[0-9a-f]{64}", value.get("id", ""))
            or not re.fullmatch(r"[0-9a-f]{64}", value.get("version", ""))
            or (value["accepted_version"] is not None
                and not re.fullmatch(r"[0-9a-f]{64}", value["accepted_version"]))
            or type(value["rendered_checked"]) is not bool
            or value["state"] not in {"pending", "accepted", "needs_re_review", "withdrawn"}
            or not isinstance(value["decision"], dict)
            or set(value["decision"]) != set(DECISION_FIELDS)):
        raise ValueError("API decision state marker has invalid state")
    # Reuse publication validation for all semantic decision fields.
    fake = {
        "status": "complete", "head_sha": "a" * 40, "base_sha": "b" * 40,
        "diff_sha256": "c" * 64, "prior_state_sha256": "d" * 64,
        "diff_base_sha": "e" * 40, "changed_files": 1, "diff_bytes": 1,
        "risk": "low", "decisions": [dict(value["decision"], prior_ids=[], relationship="new", reason="")],
    }
    validate_result(fake, "a" * 40, "b" * 40)
    if decision_id(value["decision"]) != value["version"]:
        raise ValueError("API decision state marker has a mismatched version")
    return value


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


def _record_from_marker(marker: dict, checked: bool | None) -> dict:
    state = marker["state"]
    accepted_version = marker["accepted_version"]
    if checked is not None and checked != marker["rendered_checked"]:
        state = "accepted" if checked else "pending"
        accepted_version = marker["version"] if checked else None
    elif checked:
        state = "accepted"
        accepted_version = accepted_version or marker["version"]
    record = {
        "id": marker["id"], "version": marker["version"], "state": state,
        "accepted_version": accepted_version, "decision": marker["decision"],
    }
    return record


def parse_comment_state(bodies: list[str]) -> list[dict]:
    records: list[dict] = []
    seen: set[str] = set()
    category = "other"
    headings = {f"## {title}": key for key, title in CATEGORIES.items()}
    for body in bodies:
        lines = body.splitlines()
        for index, line in enumerate(lines):
            category = headings.get(line, category)
            marker_match = STATE_MARKER_RE.search(line)
            if marker_match:
                marker = decode_state_marker(marker_match.group(1))
                checkbox = re.match(r"^- \[([ xX])\] ", line)
                checked = None if checkbox is None else checkbox.group(1).lower() == "x"
                record = _record_from_marker(marker, checked)
                if record["id"] in seen:
                    raise ValueError("API review state contains a duplicate decision identity")
                seen.add(record["id"])
                records.append(record)
                continue
            legacy = LEGACY_DECISION_RE.search(line)
            checkbox = re.match(r"^- \[([ xX])\] Accept \*\*(.*?)\*\*", line)
            if not legacy or not checkbox or legacy.group(1) in seen:
                continue
            details = {"change": "", "compatibility": ""}
            paths: list[str] = []
            for following in lines[index + 1:index + 8]:
                if following.startswith("  - **Change:** "):
                    details["change"] = following.split(":** ", 1)[1]
                elif following.startswith("  - **Compatibility:** "):
                    details["compatibility"] = following.split(":** ", 1)[1]
                elif following.startswith("  - **Code:** "):
                    paths = [urllib.parse.unquote(path) for path in re.findall(
                        r"github\.com/[^/]+/[^/]+/blob/[0-9a-f]{40}/([^#)]+)", following)]
            decision = {
                "category": category, "title": checkbox.group(2),
                "change": details["change"] or "Legacy API decision",
                "compatibility": details["compatibility"] or "Legacy compatibility state",
                "impact": "breaking" if "🔴" in line else "behavioral" if "🟡" in line else "additive",
                "locations": [{"path": path, "side": "head", "line": 0, "end_line": 0} for path in paths[:8]]
                             or [{"path": "legacy-state", "side": "head", "line": 0, "end_line": 0}],
            }
            identifier = legacy.group(1)
            seen.add(identifier)
            records.append({
                "id": identifier, "version": identifier,
                "state": "accepted" if checkbox.group(1).lower() == "x" else "pending",
                "accepted_version": identifier if checkbox.group(1).lower() == "x" else None,
                "decision": decision,
            })
    return records


def model_prior_state(records: list[dict]) -> dict:
    decisions = []
    for record in records:
        decision = record["decision"]
        decisions.append({
            "id": record["id"], "version": record["version"],
            # Acceptance is deliberately withheld from the model. Only whether
            # an identity is historical affects semantic reconciliation.
            "state": "withdrawn" if record["state"] == "withdrawn" else "pending",
            "category": decision["category"], "title": decision["title"],
            "change": decision["change"], "compatibility": decision["compatibility"],
            "impact": decision["impact"],
            "paths": sorted({location["path"] for location in decision["locations"]}),
        })
    return validate_prior_state({"version": STATE_VERSION, "decisions": decisions})


def _fresh_id(decision: dict, used: set[str]) -> str:
    version = decision_id(decision)
    counter = 0
    while True:
        value = hashlib.sha256(f"api-decision-v2:{version}:{counter}".encode()).hexdigest()
        if value not in used:
            return value
        counter += 1


def reconcile_records(result: dict, previous: list[dict] | str) -> tuple[list[dict], list[dict]]:
    if isinstance(previous, str):
        previous = parse_comment_state([previous])
    by_id = {record["id"]: record for record in previous}
    used = set(by_id)
    consumed: set[str] = set()
    current: list[dict] = []
    for decision in result["decisions"]:
        semantic = semantic_decision(decision)
        version = decision_id(semantic)
        candidates = decision["prior_ids"]
        relationship = decision["relationship"]
        if relationship == "new" and not candidates:
            exact = [record["id"] for record in previous if record["version"] == version]
            if len(exact) == 1:
                candidates, relationship = exact, "unchanged"
        prior = by_id.get(candidates[0]) if len(candidates) == 1 and relationship != "ambiguous" else None
        if prior is None:
            identifier = _fresh_id(semantic, used)
            used.add(identifier)
            state = "needs_re_review" if relationship == "ambiguous" else "pending"
            accepted_version = None
        else:
            identifier = prior["id"]
            if relationship == "unchanged" and prior["state"] != "withdrawn":
                state = prior["state"]
                accepted_version = prior["accepted_version"]
            elif relationship == "unchanged":
                state, accepted_version = "needs_re_review", None
            elif relationship == "changed":
                state, accepted_version = "needs_re_review", None
            else:
                state, accepted_version = "pending", None
        if relationship != "ambiguous":
            consumed.update(candidates)
        current.append({
            "id": identifier, "version": version, "state": state,
            "accepted_version": accepted_version, "decision": semantic,
            "reason": decision["reason"],
        })
    withdrawn = []
    for record in previous:
        if record["id"] in consumed:
            continue
        withdrawn.append({**record, "state": "withdrawn", "reason": ""})
    return current, withdrawn


def review_intro(result: dict, repo: str) -> list[str]:
    head = result["head_sha"]
    return [
        "# API decisions", "",
        f"Reviewed [{head[:12]}](https://github.com/{repo}/commit/{head}). "
        f"Input coverage: {result['changed_files']} changed files, {result['diff_bytes']:,} diff bytes "
        f"across {result.get('review_batches', 1)} complete batch(es); no truncation.",
        "",
        ("Check a box to accept a pending or changed decision. Acceptance follows materially unchanged decisions across commits; "
         + "these checkboxes do not block merging automatically."),
        "",
        f"**Testing risk:** {result['risk']}. Compatibility impact is listed separately for each decision.",
        "",
    ]


def markdown_fence(text: str) -> str:
    longest = max((len(match.group()) for match in re.finditer(r"`+", text)), default=0)
    return "`" * max(3, longest + 1)


def decision_lines(record: dict, result: dict, repo: str, diff: str = "") -> list[str]:
    decision = record["decision"]
    checked = record["state"] == "accepted"
    verb = "Accepted" if checked else "Re-review" if record["state"] == "needs_re_review" else "Accept"
    impact_symbol, impact_title = IMPACTS[decision["impact"]]
    marker = state_marker({
        "id": record["id"], "version": record["version"],
        "accepted_version": record["accepted_version"], "rendered_checked": checked,
        "state": record["state"], "decision": decision,
    })
    lines = [
        f"- [{'x' if checked else ' '}] {verb} **{inline(decision['title'])}** — "
        f"{impact_symbol} **{impact_title}**. {marker}",
        f"  - **Change:** {inline(decision['change'])}",
        f"  - **Compatibility:** {inline(decision['compatibility'])}",
        "  - **Code:** " + ", ".join(code_link(repo, result, loc) for loc in decision["locations"]),
    ]
    if record.get("reason"):
        lines.append(f"  - **Why re-review:** {inline(record['reason'])}")
    excerpt = illustrative_excerpt(diff, decision) if diff else None
    if excerpt is not None:
        lines.append("")
        if excerpt["label"]:
            lines.append(f"  **{excerpt['label']}:**")
            lines.append("")
        fence = markdown_fence(excerpt["text"])
        lines.append(f"  {fence}{excerpt['language']}")
        lines.extend(f"  {line}" for line in excerpt["text"].splitlines())
        lines.append(f"  {fence}")
    lines.append("")
    return lines


def withdrawn_lines(withdrawn: list[dict]) -> list[str]:
    if not withdrawn:
        return []
    lines = ["<details>", f"<summary>Withdrawn decisions ({len(withdrawn)})</summary>", ""]
    for record in sorted(withdrawn, key=lambda item: (item["decision"]["title"], item["id"])):
        marker = state_marker({
            "id": record["id"], "version": record["version"],
            "accepted_version": record["accepted_version"], "rendered_checked": False,
            "state": "withdrawn", "decision": record["decision"],
        })
        lines.append(f"- Withdrawn **{inline(record['decision']['title'])}**. {marker}")
    return [*lines, "", "</details>", ""]


def render_comment(result: dict, repo: str, previous: list[dict] | None = None, diff: str = "") -> str:
    current, withdrawn = reconcile_records(result, previous or [])
    lines = review_intro(result, repo)
    if not result["decisions"]:
        lines += ["No durable API decisions changed. Comments, formatting, and documentation wording alone do not require acceptance.", ""]
    for category, title in CATEGORIES.items():
        decisions = [item for item in result["decisions"] if item["category"] == category]
        lines += [f"## {title}", ""]
        if not decisions:
            lines += ["No API decisions changed.", ""]
            continue
        records = [record for record in current if record["decision"]["category"] == category]
        for record in sorted(records, key=lambda item: (item["decision"]["title"], item["id"])):
            lines += decision_lines(record, result, repo, diff)
    lines += withdrawn_lines(withdrawn)
    lines += [revision_marker(result), COMMENT_MARKER, ""]
    body = "\n".join(lines)
    if len(body.encode()) > MAX_COMMENT_BYTES - WARNING_RESERVE_BYTES:
        raise ValueError("API decision checklist exceeds GitHub's comment limit; no decisions were truncated")
    return body


def part_index(body: str) -> int:
    markers = PART_MARKER_RE.findall(body)
    if not markers:
        return 0  # Legacy single comments are primary comments.
    if len(markers) != 1:
        raise ValueError("API review comment has ambiguous part metadata")
    index, total = map(int, markers[0])
    if not 0 <= index <= total or total <= 0:
        raise ValueError("API review comment has invalid part metadata")
    return index


def render_continuations(result: dict, repo: str, previous: list[dict] | None = None, diff: str = "") -> list[str]:
    """Pack whole decision blocks; never truncate prose, evidence, or checkboxes."""
    current_records, withdrawn = reconcile_records(result, previous or [])
    limit = MAX_COMMENT_BYTES - WARNING_RESERVE_BYTES
    maximum_parts = max(1, len(result["decisions"]) + (1 if withdrawn else 0))

    def render(lines: list[str], index: int, total: int) -> str:
        return "\n".join([
            f"# API decisions — checklist part {index} of {total}", "",
            f"Reviewed revision `{result['head_sha']}`. "
            "The primary API review comment records whether every checklist part was published successfully.",
            "Check a box to accept a pending or changed decision.", "",
            *lines, revision_marker(result),
            f"<!-- ai-api-review:part={index}/{total} -->", COMMENT_MARKER, "",
        ])

    pages: list[list[str]] = []
    page_lines: list[str] = []
    current_category = None
    for category, title in CATEGORIES.items():
        records = sorted((item for item in current_records if item["decision"]["category"] == category),
                         key=lambda item: (item["decision"]["title"], item["id"]))
        for record in records:
            block = decision_lines(record, result, repo, diff)
            heading = [f"## {title}", ""]
            candidate = page_lines + ([] if current_category == category else heading) + block
            # Reserve the maximum possible page-number width before any write.
            if len(render(candidate, maximum_parts, maximum_parts).encode()) > limit:
                if page_lines:
                    pages.append(page_lines)
                page_lines = heading + block
                if len(render(page_lines, maximum_parts, maximum_parts).encode()) > limit:
                    raise ValueError("An individual API decision exceeds GitHub's comment limit; no decisions were truncated")
            else:
                page_lines = candidate
            current_category = category
    history = withdrawn_lines(withdrawn)
    if history:
        candidate = page_lines + history
        if page_lines and len(render(candidate, maximum_parts, maximum_parts).encode()) > limit:
            pages.append(page_lines)
            page_lines = history
        else:
            page_lines = candidate
        if len(render(page_lines, maximum_parts, maximum_parts).encode()) > limit:
            raise ValueError("Withdrawn API decision history exceeds GitHub's comment limit")
    if page_lines:
        pages.append(page_lines)
    if not pages:
        raise ValueError("Cannot partition an empty API decision checklist")
    return [render(lines, index, len(pages)) for index, lines in enumerate(pages, start=1)]


def render_primary(result: dict, repo: str, pr_number: int, comment_ids: list[int]) -> str:
    lines = review_intro(result, repo) + [
        f"**Complete checklist:** {len(result['decisions'])} decisions across {len(comment_ids)} comments.", "",
        *[f"- [Checklist part {index} of {len(comment_ids)}](https://github.com/{repo}/pull/{pr_number}#issuecomment-{identifier})"
          for index, identifier in enumerate(comment_ids, start=1)],
        "", revision_marker(result),
        f"<!-- ai-api-review:part=0/{len(comment_ids)} -->", COMMENT_MARKER, "",
    ]
    body = "\n".join(lines)
    if len(body.encode()) > MAX_COMMENT_BYTES - WARNING_RESERVE_BYTES:
        raise ValueError("API checklist index exceeds GitHub's comment limit; no decisions were truncated")
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


def authoritative_records(comments: list[dict]) -> list[dict]:
    comments = sorted(comments, key=lambda comment: comment["id"])
    primary = next((comment for comment in comments if part_index(comment["body"]) == 0), None)
    if primary is None:
        return []
    bodies = [primary["body"]]
    linked_ids = {int(value) for value in re.findall(r"#issuecomment-(\d+)", primary["body"])}
    if linked_ids:
        by_id = {comment["id"]: comment for comment in comments}
        if any(identifier not in by_id for identifier in linked_ids):
            raise ValueError("API review primary links to missing checklist state")
        bodies.extend(by_id[identifier]["body"] for identifier in sorted(linked_ids))
    return parse_comment_state(bodies)


def list_review_comments(repo: str, pr_number: int, github) -> list[dict]:
    issue = f"/repos/{repo}/issues/{pr_number}"
    existing = []
    for page in range(1, 101):
        comments = github.request("GET", f"{issue}/comments?per_page=100&page={page}")
        if not isinstance(comments, list):
            raise ValueError("GitHub returned invalid API review comments")
        existing.extend(comment for comment in comments if (
            (comment.get("user") or {}).get("login") == "github-actions[bot]"
            and (comment.get("user") or {}).get("type") == "Bot"
            and COMMENT_MARKER in (comment.get("body") or "")
        ))
        if len(comments) < 100:
            return sorted(existing, key=lambda comment: comment["id"])
    raise ValueError("Could not enumerate all previous API review comments")


def fetch_state(repo: str, pr_number: int, output: Path, github) -> None:
    if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repo) or pr_number <= 0:
        raise ValueError("Invalid repository or PR number")
    state = model_prior_state(authoritative_records(list_review_comments(repo, pr_number, github)))
    output.write_text(json.dumps(state, indent=2, sort_keys=True) + "\n", encoding="utf-8")


def publish(result: dict, repo: str, pr_number: int, head_sha: str, base_sha: str, diff_bytes: bytes, github) -> bool:
    if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repo) or pr_number <= 0:
        raise ValueError("Invalid repository or PR number")
    validate_result(result, head_sha, base_sha)
    diff = ""
    if result["status"] == "complete":
        if len(diff_bytes) != result["diff_bytes"] or hashlib.sha256(diff_bytes).hexdigest() != result["diff_sha256"]:
            raise ValueError("API review diff does not match the complete result")
        try:
            diff = diff_bytes.decode("utf-8")
        except UnicodeDecodeError as error:
            raise ValueError("API review diff is not valid UTF-8") from error
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
    existing = list_review_comments(repo, pr_number, github)
    primary = next((comment for comment in existing if part_index(comment["body"]) == 0), None)
    previous = primary["body"] if primary else ""
    complete = result["status"] == "complete"
    continuations = []
    try:
        prior_records = authoritative_records(existing)
        if complete and prior_state_digest(model_prior_state(prior_records)) != result["prior_state_sha256"]:
            raise ValueError("API review state changed during reconciliation; prior acceptance was preserved")
        if complete:
            try:
                body = render_comment(result, repo, prior_records, diff)
            except ValueError:
                continuations = render_continuations(result, repo, prior_records, diff)
                # GitHub comment IDs are bounded integers. Validate the complete
                # primary with worst-case link lengths before creating any part.
                body = render_primary(result, repo, pr_number, [10**20 - 1] * len(continuations))
        else:
            body = render_incomplete(result, repo, previous)
    except ValueError as error:
        if not complete:
            raise
        result = {**result, "status": "incomplete", "error": str(error)}
        complete = False
        continuations = []
        body = render_incomplete(result, repo, previous)
    # A model call can take minutes. Recheck after reading state and directly
    # before mutations so a superseded run cannot overwrite a newer checklist.
    pull = current_pull()
    primary_attempted = False
    try:
        if continuations:
            identifiers = []
            # Fresh continuations keep the previous primary's linked checklist
            # intact if this attempt fails halfway through publication.
            for continuation in continuations:
                created = github.request("POST", f"{issue}/comments", {"body": continuation})
                identifier = created.get("id") if isinstance(created, dict) else None
                if type(identifier) is not int or not 0 < identifier < 10**20:
                    raise ValueError("GitHub returned an invalid checklist comment ID")
                identifiers.append(identifier)
            body = render_primary(result, repo, pr_number, identifiers)
            pull = current_pull()
        primary_attempted = True
        if primary:
            github.request("PATCH", f"{root}/issues/comments/{primary['id']}", {"body": body})
        else:
            github.request("POST", f"{issue}/comments", {"body": body})
    except (ValueError, OSError, urllib.error.URLError):
        if not continuations:
            raise
        # Never touch a newly superseding revision. A failed POST of the primary
        # may already have created it, so do not retry that ambiguous creation.
        current_pull()
        if not primary and primary_attempted:
            raise
        complete = False
        result = {**result, "status": "incomplete", "error": "Could not publish every API checklist part; prior review state was preserved"}
        body = render_incomplete(result, repo, previous)
        if primary:
            github.request("PATCH", f"{root}/issues/comments/{primary['id']}", {"body": body})
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
    # Only a fully published replacement may remove old continuation pages.
    # Primary comments are identified explicitly, never by creation order.
    for comment in existing:
        if part_index(comment["body"]) > 0:
            try:
                github.request("DELETE", f"{root}/issues/comments/{comment['id']}")
            except urllib.error.HTTPError as error:
                if error.code != 404:
                    raise
    return True


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    fetch = commands.add_parser("fetch-state")
    fetch.add_argument("--repo", required=True)
    fetch.add_argument("--pr-number", required=True, type=int)
    fetch.add_argument("--output", required=True)
    publish_parser = commands.add_parser("publish")
    for option in ("result", "repo", "expected-head-sha", "expected-base-sha"):
        publish_parser.add_argument("--" + option, required=True)
    publish_parser.add_argument("--pr-number", required=True, type=int)
    args = parser.parse_args()
    token = os.environ.get("GH_TOKEN")
    if not token:
        raise ValueError("GH_TOKEN is required for API review state")
    github = GitHub(token)
    if args.command == "fetch-state":
        fetch_state(args.repo, args.pr_number, Path(args.output), github)
        return 0
    result_path = Path(args.result)
    result = json.loads(result_path.read_text())
    # SECURITY: Both fixed-name files are created in RUNNER_TEMP by trusted
    # base-revision automation, with no PR-controlled steps. Keep this interface
    # unchanged during rollout, then verify the immutable diff fingerprint
    # before rendering any PR-controlled source text.
    diff_bytes = result_path.with_name("api-review-pr.diff").read_bytes()
    return 0 if publish(result, args.repo, args.pr_number, args.expected_head_sha,
                        args.expected_base_sha, diff_bytes, github) else 1


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (ValueError, OSError, urllib.error.URLError) as error:
        print(f"API review publication failed: {error}", file=sys.stderr)
        sys.exit(1)
