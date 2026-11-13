"""Advisory finding history in the existing API review comment.

This stores observations, never approval. Reassessment happens in the existing
evidence call; missing/partial assessment retains the prior concern visibly.
"""
from __future__ import annotations

import base64
import copy
import hashlib
import json
import re
import zlib

from review_sources import ReviewError, valid_path, is_generated_path

MARKER = re.compile(r"<!-- compatibility-finding:v1:([A-Za-z0-9_-]+) -->")
STATES = {"open", "not_assessed", "addressed", "cancelled"}
MAX_RECORD_BYTES = 24_000
MAX_STATE_BYTES = 96_000
MAX_RECORDS = 100
FINDING_FIELDS = {"assessment", "direction", "consumer", "action", "condition", "before", "after", "consider", "changed", "caller", "receiver"}


def validate_records(records, *, public_only=True):
    if not isinstance(records, list) or len(records) > MAX_RECORDS:
        raise ReviewError("Compatibility history exceeds its bounded record limit")
    if len(json.dumps(records, ensure_ascii=False).encode()) > MAX_STATE_BYTES:
        raise ReviewError("Compatibility history exceeds its byte limit; prior review was preserved")
    seen = set()
    for record in records:
        if not isinstance(record, dict) or set(record) != {"id", "status", "finding", "resolution"}:
            raise ReviewError("Invalid prior compatibility record")
        identifier, status, finding, resolution = (record[k] for k in ("id", "status", "finding", "resolution"))
        if (not isinstance(identifier, str) or not re.fullmatch(r"[a-f0-9]{64}", identifier)
                or identifier in seen or not isinstance(status, str) or status not in STATES):
            raise ReviewError("Invalid prior compatibility identity/status")
        seen.add(identifier)
        if not isinstance(finding, dict) or set(finding) != FINDING_FIELDS:
            raise ReviewError("Invalid prior compatibility finding")
        for field in ("consumer", "action", "condition", "before", "after", "consider"):
            if not isinstance(finding[field], str) or not finding[field].strip() or len(finding[field]) > 1200:
                raise ReviewError("Invalid prior compatibility text")
        if (not isinstance(finding["assessment"], str) or finding["assessment"] not in {"breaking", "migration_needed"}
                or not isinstance(finding["direction"], str) or finding["direction"] not in {"old_caller_new_peer", "new_caller_old_peer", "state_reader_writer"}):
            raise ReviewError("Invalid prior compatibility classification")
        if not isinstance(resolution, dict) or set(resolution) != {"reason", "evidence"} or not isinstance(resolution["reason"], str) or len(resolution["reason"]) > 1600:
            raise ReviewError("Invalid prior compatibility resolution")
        for refs in [finding[k] for k in ("changed", "caller", "receiver")] + [resolution["evidence"]]:
            if not isinstance(refs, list) or len(refs) > 8:
                raise ReviewError("Invalid prior compatibility evidence")
            for ref in refs:
                if (not isinstance(ref, dict) or ref.get("visibility") not in ({"public"} if public_only else {"public", "private"})
                        or not isinstance(ref.get("repository"), str) or not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", ref["repository"])
                        or not isinstance(ref.get("revision"), str) or not re.fullmatch(r"[a-f0-9]{40}", ref["revision"])
                        or not valid_path(ref.get("path")) or is_generated_path(ref["path"])
                        or type(ref.get("line")) is not int or type(ref.get("end_line")) is not int
                        or not 0 <= ref["line"] <= ref["end_line"]):
                    raise ReviewError("Invalid or private prior compatibility provenance")
    return records


def marker(record):
    raw = json.dumps(record, sort_keys=True, separators=(",", ":")).encode()
    if len(raw) > MAX_RECORD_BYTES:
        raise ReviewError("Compatibility finding state exceeds its marker limit")
    value = base64.urlsafe_b64encode(zlib.compress(raw, 9)).decode().rstrip("=")
    return "<!-- compatibility-finding:v1:" + value + " -->"


def unfenced_lines(body):
    """Source examples and quoted markers are data, never review state."""
    fence = None
    for index, line in enumerate(body.splitlines()):
        match = re.match(r"^[ \t]{0,3}(`{3,}|~{3,})(.*)$", line)
        if fence:
            if match and match[1][0] == fence[0] and len(match[1]) >= len(fence) and not match[2].strip():
                fence = None
            continue
        if match:
            fence = match[1]
            continue
        yield index, line


def parse(bodies):
    records = []
    for body in bodies:
        for _, line in unfenced_lines(body):
            match = MARKER.fullmatch(line)
            if not match:
                continue
            if len(match[1]) > 32_000:
                raise ReviewError("Compatibility state marker is too large")
            try:
                raw = base64.urlsafe_b64decode(match[1] + "=" * (-len(match[1]) % 4))
                decoder = zlib.decompressobj()
                data = decoder.decompress(raw, MAX_RECORD_BYTES + 1)
                if len(data) > MAX_RECORD_BYTES or decoder.unconsumed_tail or decoder.unused_data or not decoder.eof:
                    raise ValueError("oversized marker")
                records.append(json.loads(data))
            except (ValueError, TypeError, zlib.error) as error:
                raise ReviewError("Invalid compatibility history marker") from error
    return validate_records(records)


def current(result):
    """Apply explicit source-backed updates; absence is never resolution."""
    public_only = result.get("input_visibility") != "private"
    previous = validate_records(copy.deepcopy(result.get("compatibility_previous", [])), public_only=public_only)
    by_id = {record["id"]: record for record in previous}
    used = set()
    for finding in result.get("compatibility_findings", []):
        identifier = finding.get("prior_id", "")
        stored = {k: finding[k] for k in FINDING_FIELDS}
        if not identifier:
            seed = json.dumps(stored, sort_keys=True, separators=(",", ":"))
            identifier = hashlib.sha256(("compatibility-v1:" + seed).encode()).hexdigest()
        if identifier in used:
            raise ReviewError("Multiple compatibility findings claim one identity")
        if finding.get("prior_id") and identifier not in by_id:
            raise ReviewError("Compatibility finding claims an unknown prior identity")
        used.add(identifier)
        by_id[identifier] = {"id": identifier, "status": "open", "finding": stored,
                             "resolution": {"reason": "", "evidence": []}}
    for update in result.get("compatibility_updates", []):
        identifier = update["prior_id"]
        if identifier not in by_id or identifier in used:
            raise ReviewError("Invalid/conflicting compatibility history update")
        used.add(identifier)
        by_id[identifier].update(status=update["status"], resolution={"reason": update["reason"], "evidence": update["evidence"]})
    for record in previous:
        if record["status"] in {"open", "not_assessed"} and record["id"] not in used:
            by_id[record["id"]].update(status="not_assessed", resolution={
                "reason": "This pass did not reassess the prior concern; it has not been cleared.", "evidence": []})
    return validate_records(sorted(by_id.values(), key=lambda r: (r["status"] not in {"open", "not_assessed"}, r["id"])), public_only=public_only)
