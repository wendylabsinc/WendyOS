"""One fixed read-only page over a bounded projection of the live transcript."""

import datetime, html, http.server, json, pathlib, re, time
from common import scrub


def text_event(e):
    kind = e.get("type", "")
    if kind == "message_end":
        if e.get("message", {}).get("role") != "assistant":
            return ""
        return "Model narration (unverified): " + "\n".join(
            c.get("text", "")
            for c in e.get("message", {}).get("content", [])
            if c.get("type") == "text"
        )
    if kind == "tool_execution_start":
        args = e.get("args", {})
        safe = {
            k: v
            for k, v in args.items()
            if k in {"name", "operation", "kind", "version", "predicate", "field"}
        }
        script = str(args.get("text", ""))[:2400]
        return (
            "Tool call: "
            + str(e.get("toolName", ""))
            + " "
            + json.dumps(safe)
            + ("\nSubmitted script:\n" + script if script else "")
        )
    if kind == "tool_execution_end":
        result = e.get("result", {})
        try:
            value = json.loads(
                next(
                    c["text"]
                    for c in result.get("content", [])
                    if c.get("type") == "text"
                )
            )
        except (StopIteration, ValueError, KeyError, TypeError):
            value = {}
        safe = {
            k: value[k]
            for k in [
                "exit",
                "seconds",
                "verified",
                "kind",
                "version",
                "command",
                "reason",
            ]
            if k in value
        }
        # Worker sandbox output is credential-free; control private raw probes
        # never enter tool results. Redact and escape again for the browser.
        output = ""
        if value.get("evidence_kind") == "offline_sandbox":
            output = (
                "\nWorker stdout:\n"
                + str(value.get("output", ""))[:2400]
                + "\nWorker file output:\n"
                + str(value.get("file_output", ""))[:800]
            )
        elif value.get("evidence_kind") == "fresh_command":
            output = (
                "\nProjected probe output:\n"
                + json.dumps(value.get("output", {}))[:1600]
            )
        return (
            "Tool result: "
            + str(e.get("toolName", ""))
            + " "
            + json.dumps(safe)
            + output
        )
    if kind in {"session_started", "session_finished"}:
        return (
            kind
            + " "
            + json.dumps({k: e[k] for k in ["exit", "proof", "seconds"] if k in e})
        )
    return ""


def render(work, state, now=None):
    now = time.time() if now is None else now
    work = pathlib.Path(work)
    runs = sorted(
        p for p in (work / "runs").glob("*") if p.is_dir() and not p.is_symlink()
    )
    run = runs[-1] if runs else None
    rows = []
    entries = []
    if (
        run
        and (run / "events.jsonl").is_file()
        and not (run / "events.jsonl").is_symlink()
    ):
        with (run / "events.jsonl").open("rb") as f:
            f.seek(0, 2)
            size = f.tell()
            f.seek(max(0, size - 128 * 1024))
            data = f.read(128 * 1024)
        for line in data.splitlines():
            try:
                e = json.loads(line)
            except (ValueError, UnicodeDecodeError):
                continue
            entries.append(e)
            text = text_event(e)
            if text:
                rows.append((str(e.get("timestamp_utc", "")), str(scrub(text))[:4000]))
    held = work / "held.json"
    label = "running"
    detail = ""
    elapsed = "—"
    if state.get("stopping"):
        label = "stopping"
    elif held.exists():
        label = "blocked"
        try:
            detail = str(scrub(json.loads(held.read_text()[:4096]).get("reason", "")))[
                :500
            ]
        except ValueError:
            detail = "Worker held; inspect private operator logs"
    elif state.get("error"):
        label = "blocked"
        detail = str(scrub(state["error"]))[:500]
    elif not state.get("ready"):
        label = "starting"
    elif not run or (entries and entries[-1].get("type") == "session_finished"):
        label = "idle"
    elif state.get("worker_age", float("inf")) > 10:
        label = "stale"
    if run:
        try:
            start = float(run.name.split("-", 1)[0])
        except ValueError:
            start = now
        try:
            first = next(e for e in entries if e.get("type") == "session_started")
            start = datetime.datetime.fromisoformat(first["timestamp_utc"]).timestamp()
        except (StopIteration, KeyError, ValueError):
            pass
        end = now
        if entries and entries[-1].get("type") == "session_finished":
            elapsed = f"{float(entries[-1].get('seconds', 0)):.1f}s (completed)"
        else:
            elapsed = f"{max(0, int(end - start))}s"
    esc = lambda x: html.escape(str(x), quote=True)
    transcript = (
        "".join(
            "<p>" + esc(at) + "</p><pre>" + esc(text) + "</pre>"
            for at, text in rows[-24:]
        )
        or "<p>No visible transcript events yet.</p>"
    )
    return (
        '<!doctype html><html><head><meta charset="utf-8"><meta http-equiv="refresh" content="5"><title>Wendy Scout PoC</title></head><body>'
        "<h1>Wendy Scout PoC</h1><p>State: <strong>"
        + esc(label)
        + "</strong> — elapsed "
        + esc(elapsed)
        + "</p><p>"
        + esc(detail)
        + "</p>"
        "<p>Session: "
        + esc(run.name if run else "none")
        + "; last visible event: "
        + esc(rows[-1][0] if rows else "none")
        + "</p>"
        "<p>Model text is unverified narration. Tool results below are a limited public projection. Refreshes every five seconds. A stopped app cannot serve this page.</p>"
        "<details open><summary>Latest visible transcript</summary>"
        + transcript
        + "</details></body></html>"
    ).encode()


def start(work, state, port=8087):
    class Handler(http.server.BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def do_GET(self):
            if self.path != "/":
                self.send_error(404)
                return
            body = render(work, state())
            self.send_response(200)
            self.send_header("Content-Type", "text/html; charset=utf-8")
            self.send_header("Content-Length", str(len(body)))
            self.send_header("Cache-Control", "no-store")
            self.send_header("X-Content-Type-Options", "nosniff")
            self.send_header(
                "Content-Security-Policy",
                "default-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'",
            )
            self.end_headers()
            self.wfile.write(body)

        def do_POST(self):
            self.send_error(405)

    return http.server.HTTPServer(("0.0.0.0", port), Handler)
