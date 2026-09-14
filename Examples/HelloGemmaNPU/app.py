#!/usr/bin/env python3
"""HelloGemmaNPU - text generation on the Hexagon NPU via Genie and the QNN HTP backend.

Prints an NPU report at startup, then serves a web page for interactive prompts.
"""

import html
import json
import os
import shutil
import subprocess
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

APP_DIR = os.path.dirname(os.path.abspath(__file__))
CONFIG = os.path.join(APP_DIR, "genie_config.json")
MODEL_DIR = os.path.join(APP_DIR, "model")
PORT = int(os.environ.get("PORT", "8080"))
TIMEOUT_S = int(os.environ.get("GENIE_TIMEOUT", "300"))

# Agents older than the unsigned-PD entitlement fix do not supply this, and the
# non-secure FastRPC nodes a container receives accept no other process domain.
os.environ.setdefault("FASTRPC_PROCESS_ATTRS", "8")


def npu_report():
    """What the entitlement actually delivered, as name/value pairs."""
    nodes = sorted(
        n for n in os.listdir("/dev") if n.startswith("fastrpc-")
    ) if os.path.isdir("/dev") else []
    return [
        ("FastRPC nodes", " ".join(nodes) or "<none - is the npu entitlement declared?>"),
        ("MACHINE_NAME", os.environ.get("MACHINE_NAME", "<unset>")),
        ("FASTRPC_PROCESS_ATTRS", os.environ.get("FASTRPC_PROCESS_ATTRS", "<unset>")),
        ("dma-buf heap", "present" if os.path.exists("/dev/dma_heap/system") else "missing"),
        ("genie-t2t-run", shutil.which("genie-t2t-run") or "<not on PATH>"),
    ]


def missing_pieces():
    """Which staged files are absent. Empty means the demo can generate."""
    missing = []
    if not os.path.exists(CONFIG):
        missing.append("genie_config.json")
    if not os.path.isdir(MODEL_DIR) or not any(
        f.endswith(".serialized.bin") for f in os.listdir(MODEL_DIR)
    ):
        missing.append("model/*.serialized.bin")
    if not os.path.exists(os.path.join(MODEL_DIR, "tokenizer.json")):
        missing.append("model/tokenizer.json")
    return missing


def generate(prompt):
    """Run one prompt through Genie. Returns (output_text, seconds)."""
    started = time.monotonic()
    # List form, never a shell: the prompt is untrusted input from the page.
    proc = subprocess.run(
        ["genie-t2t-run", "-c", CONFIG, "-p", prompt],
        cwd=APP_DIR,
        capture_output=True,
        text=True,
        timeout=TIMEOUT_S,
    )
    elapsed = time.monotonic() - started
    out = (proc.stdout or "") + (proc.stderr or "")
    if proc.returncode != 0 and not out.strip():
        out = f"genie-t2t-run exited {proc.returncode} with no output"
    return out.strip(), elapsed


PAGE = """<!doctype html>
<meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Gemma on the Hexagon NPU</title>
<style>
 :root {{ color-scheme: light dark; }}
 body {{ font: 15px/1.5 system-ui, sans-serif; margin: 0; padding: 24px;
        max-width: 780px; margin-inline: auto; }}
 h1 {{ font-size: 20px; margin: 0 0 4px; }}
 .sub {{ opacity: .7; margin-bottom: 20px; }}
 table {{ border-collapse: collapse; font-size: 13px; margin-bottom: 20px; width: 100%; }}
 td {{ padding: 4px 10px 4px 0; vertical-align: top; }}
 td:first-child {{ opacity: .7; white-space: nowrap; }}
 textarea {{ width: 100%; box-sizing: border-box; font: inherit; padding: 10px;
             border-radius: 8px; border: 1px solid rgba(128,128,128,.5); }}
 button {{ margin-top: 10px; font: inherit; padding: 8px 18px; border-radius: 8px;
           border: 0; background: #2f6feb; color: #fff; cursor: pointer; }}
 pre {{ white-space: pre-wrap; word-wrap: break-word; padding: 14px; border-radius: 8px;
        background: rgba(128,128,128,.12); }}
 .warn {{ padding: 14px; border-radius: 8px; background: rgba(220,160,0,.15); }}
</style>
<h1>Gemma on the Hexagon NPU</h1>
<div class="sub">Genie + QNN HTP backend, inside a container deployed with <code>wendy run</code>.</div>
<table>{report}</table>
{body}
"""

FORM = """<form method="post" action="/generate">
 <textarea name="prompt" rows="3" autofocus>{prompt}</textarea>
 <button type="submit">Generate on the NPU</button>
</form>
{result}
"""


def render(body):
    rows = "".join(
        f"<tr><td>{html.escape(k)}</td><td><code>{html.escape(str(v))}</code></td></tr>"
        for k, v in npu_report()
    )
    return PAGE.format(report=rows, body=body).encode()


def page(prompt="", result=""):
    missing = missing_pieces()
    if missing:
        return render(
            '<div class="warn"><b>No model staged.</b> Missing: <code>'
            + html.escape(", ".join(missing))
            + "</code><br>Stage a Genie bundle and redeploy:<br>"
            + "<code>MODEL_DIR=/path/to/export ./setup.sh &lt;device-ip&gt;</code><br>"
            + "<code>wendy run --device &lt;device-ip&gt;</code></div>"
        )
    return render(FORM.format(prompt=html.escape(prompt), result=result))


class Handler(BaseHTTPRequestHandler):
    def _send(self, payload, status=200):
        self.send_response(status)
        self.send_header("Content-Type", "text/html; charset=utf-8")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def do_GET(self):
        self._send(page())

    def do_POST(self):
        length = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(length).decode("utf-8", "replace")
        prompt = ""
        for pair in raw.split("&"):
            key, _, value = pair.partition("=")
            if key == "prompt":
                from urllib.parse import unquote_plus

                prompt = unquote_plus(value).strip()
        if not prompt:
            self._send(page())
            return
        try:
            out, secs = generate(prompt)
            result = (
                f"<pre>{html.escape(out)}</pre>"
                f'<div class="sub">{secs:.1f} s on the NPU</div>'
            )
        except subprocess.TimeoutExpired:
            result = f'<div class="warn">Timed out after {TIMEOUT_S}s.</div>'
        self._send(page(prompt, result))

    def log_message(self, fmt, *args):
        print("http: " + fmt % args)


def main():
    print("=" * 64)
    print("  HelloGemmaNPU - Gemma on the Hexagon NPU")
    print("=" * 64)
    for key, value in npu_report():
        print(f"  {key:24} {value}")
    missing = missing_pieces()
    print(f"  {'model':24} {'staged' if not missing else 'MISSING: ' + ', '.join(missing)}")
    print(f"\n  Open http://<device>:{PORT}/ and enter a prompt.\n")
    ThreadingHTTPServer(("0.0.0.0", PORT), Handler).serve_forever()


if __name__ == "__main__":
    main()
