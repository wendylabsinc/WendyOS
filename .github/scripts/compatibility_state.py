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

from review_sources import ReviewError
import compatibility_review as compat

MARKER = re.compile(r"<!-- compatibility-finding:v1:([A-Za-z0-9_-]+) -->")
STATES = {"open", "not_assessed", "addressed", "cancelled"}
MAX_RECORD_BYTES = 24_000
MAX_STATE_BYTES = 96_000
MAX_RECORDS = 100
FINDING_FIELDS = compat.FINDING_FIELDS


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
        compat.validate_finding(finding, public_only=public_only)
        compat.validate_resolution(resolution, status, public_only=public_only)
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


def finding_id(finding):
    """The existing history identity, shared with human-acceptance checks."""
    if finding.get("prior_id"):
        return finding["prior_id"]
    stored = {k: finding[k] for k in FINDING_FIELDS}
    seed = json.dumps(stored, sort_keys=True, separators=(",", ":"))
    return hashlib.sha256(("compatibility-v1:" + seed).encode()).hexdigest()


def current(result, *, bindings=None):
    """Apply explicit source-backed updates; absence is never resolution."""
    public_only = result.get("input_visibility") != "private"
    previous = validate_records(copy.deepcopy(result.get("compatibility_previous", [])), public_only=public_only)
    by_id = {record["id"]: record for record in previous}
    used = set()
    reassessed = {update["prior_id"] for update in result.get("compatibility_updates", [])}
    for finding in result.get("compatibility_findings", []):
        identifier = finding_id(finding)
        # An unbound observation must not overwrite a separately reassessed
        # historical record, even when its content hashes to that old identity.
        if not finding.get("prior_id") and identifier in by_id and identifier in reassessed:
            canonical, counter = identifier, 0
            while identifier in by_id or identifier in used:
                identifier = hashlib.sha256(f"compatibility-v1:unbound:{canonical}:{counter}".encode()).hexdigest()
                counter += 1
        stored = {k: finding[k] for k in FINDING_FIELDS}
        if identifier in used:
            raise ReviewError("Multiple compatibility findings claim one identity")
        if finding.get("prior_id") and identifier not in by_id:
            raise ReviewError("Compatibility finding claims an unknown prior identity")
        used.add(identifier)
        if bindings is not None:
            bindings[json.dumps(finding, sort_keys=True)] = identifier
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
