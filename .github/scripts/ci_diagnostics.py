"""Fail-closed summaries for untrusted CLI stderr; never pass raw text to CI."""

import json
import re


LIMIT = 65536
# Only fixed vocabulary reaches the log. A general token/PEM denylist cannot
# safely distinguish an arbitrary server message from an unlabelled secret.
CODES = frozenset((
    "error", "auth_login_expired", "device_identity_mismatch",
    "connection_failed", "permission_denied", "unauthenticated",
    "deadline_exceeded", "unavailable", "cancelled", "no_device",
    "device_org_mismatch", "device_tls_rejected", "device_auth_required",
    "device_not_resolved", "device_unreachable", "auth_required",
    "auth_session_ambiguous", "auth_certificate_failed", "cli_usage",
))
SIGNALS = (
    ("expected device identity required", "expected device identity required"),
    ("TLS authentication failed", "TLS authentication failed"),
    ("certificate has expired", "certificate has expired"),
    ("remote error: tls: bad certificate", "remote TLS alert: bad certificate"),
    ("certificate is not valid for client authentication", "certificate lacks client authentication"),
    ("connection refused", "connection refused"),
    ("no such host", "hostname resolution failed"),
    ("context deadline exceeded", "context deadline exceeded"),
    ("i/o timeout", "I/O timeout"),
    ("PermissionDenied", "gRPC PermissionDenied"),
    ("Unauthenticated", "gRPC Unauthenticated"),
    ("Unavailable", "gRPC Unavailable"),
)


def stderr_summary(raw):
    if not raw:
        return "empty stderr"
    if isinstance(raw, bytes):
        raw = raw.decode("utf-8", errors="replace")
    if len(raw) > LIMIT:
        return "[redacted: stderr exceeds 64 KiB]"
    # Do not inspect PEM material, even if it happens to contain known words.
    raw = re.sub(r"-----BEGIN [^-\r\n]+-----.*?(?:-----END [^-\r\n]+-----|$)",
                 "", raw, flags=re.DOTALL)
    code = None
    message = raw
    try:
        payload = json.loads(raw)
        error = payload.get("error", payload) if isinstance(payload, dict) else None
        if isinstance(error, dict):
            candidate = error.get("code")
            code = candidate if isinstance(candidate, str) and candidate in CODES else None
            message = error.get("message", "")
            if not isinstance(message, str):
                message = ""
    except (ValueError, RecursionError):
        pass
    signals = [safe for needle, safe in SIGNALS if needle in message]
    if "Your Wendy login for " in message and " expired at " in message:
        signals.append("Wendy login expiry reported (context and date withheld)")
    details = "; ".join((["code=" + code] if code else []) + signals)
    return (details + "; " if details else "") + "[redacted: all other stderr withheld]"


def failure_summary(error):
    # Never stringify exceptions: CalledProcessError includes command arguments,
    # JSON errors include offending input, and OSError may include private paths.
    kind = type(error).__name__
    status = getattr(error, "returncode", None)
    exit_code = f" exit={status}" if isinstance(status, int) else ""
    return f"{kind}{exit_code}; CLI stderr: {stderr_summary(getattr(error, 'stderr', None))}"
