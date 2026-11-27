#!/usr/bin/env python3
"""Build and run Go2 chat evals against in-memory robot state only."""
import argparse
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import urllib.parse
import urllib.request

ROOT = Path(__file__).resolve().parents[2]


def loopback_url(value):
    parsed = urllib.parse.urlsplit(value)
    try:
        valid = ipaddress.ip_address(parsed.hostname).is_loopback
        port = parsed.port
    except (ValueError, TypeError):
        valid, port = False, None
    if (not valid or not port or parsed.scheme != "http" or parsed.username
            or parsed.password or parsed.query or parsed.fragment
            or parsed.path.rstrip("/") != "/v1"):
        raise argparse.ArgumentTypeError("use http://<literal loopback IP>:<port>/v1")
    return value.rstrip("/")


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        raise RuntimeError("model metadata redirects are forbidden")


def model_metadata(url, model):
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
    base = url.removesuffix("/v1")
    def get(path):
        with opener.open(base + path, timeout=10) as response:
            return json.load(response)
    matches = [row for row in get("/api/tags")["models"] if row["name"] == model]
    if len(matches) != 1:
        raise RuntimeError(f"Ollama model {model!r} must already be installed")
    return {"runtime": get("/api/version"), "model": matches[0]}


def summarize_reports(directory, mode, expected):
    prefix = "real-model" if mode == "model" else "scripted-plumbing"
    reports = [json.loads(path.read_text()) for path in directory.glob(prefix + "-*.json")]
    actual = [report["case"]["id"] for report in reports]
    if len(actual) != len(expected) or set(actual) != set(expected):
        raise RuntimeError(f"missing or unexpected eval reports: expected {sorted(expected)}, got {sorted(actual)}")
    return {"passed": sum(report["passed"] is True for report in reports),
            "failed": sum(report["passed"] is not True for report in reports),
            "cases": [{"id": report["case"]["id"], "passed": report["passed"],
                       "violations": report["violations"]} for report in reports]}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--mode", choices=["scripted", "model"], default="scripted")
    parser.add_argument("--model", default="qwen3-vl:4b-instruct")
    parser.add_argument("--model-url", type=loopback_url, default="http://127.0.0.1:11434/v1")
    parser.add_argument("--expected-digest", help="fail if installed Ollama model differs")
    parser.add_argument("--cases", default="", help="comma-separated case IDs; empty runs all")
    parser.add_argument("--repetitions", type=int, default=1)
    parser.add_argument("--timeout-seconds", type=int, default=120, help="model case budget, 1..600 seconds")
    parser.add_argument("--output", type=Path, help="new directory outside the repository")
    parser.add_argument("--prepare-only", action="store_true", help="export schemas and build test binary")
    args = parser.parse_args()
    if not 1 <= args.repetitions <= 20:
        parser.error("repetitions must be between 1 and 20")
    if not 1 <= args.timeout_seconds <= 600:
        parser.error("timeout-seconds must be between 1 and 600")
    out = args.output.expanduser().resolve() if args.output else Path(tempfile.mkdtemp(prefix="go2-eval-"))
    if out == ROOT or ROOT in out.parents:
        parser.error("reports must be outside the repository")
    if args.output:
        out.mkdir(parents=True, exist_ok=False)
    print(f"Reports: {out}", flush=True)
    env = os.environ.copy()
    # A caller's model credentials/device configuration must never affect an eval.
    for key in list(env):
        if key.startswith(("WENDY_", "OPENAI_", "ANTHROPIC_")):
            del env[key]
    env["WENDY_GO2_EVAL_CATALOG"] = str(out / "catalog.json")
    subprocess.run(["go", "test", "./go/internal/cli/mcp", "-run", "^TestExportGo2EvalCatalog$",
                    "-count=1", "-timeout=2m"], cwd=ROOT, env=env, check=True)
    build_env = dict(env, CGO_ENABLED="0")
    subprocess.run(["go", "test", "-c", "-o", str(out / "chat.test"),
                    "./go/internal/cli/chat"], cwd=ROOT, env=build_env, check=True)
    shutil.copytree(ROOT / "go/internal/cli/chat/testdata/go2-eval", out / "testdata/go2-eval")
    fixture = json.loads((out / "testdata/go2-eval/cases.json").read_text())
    known = {case["id"] for case in fixture}
    expected = set(args.cases.split(",")) if args.cases else known
    if not expected or not expected <= known:
        raise RuntimeError("no cases selected or unknown case IDs supplied")
    metadata = {"mode": args.mode, "cases": args.cases or "all", "repetitions": args.repetitions,
                "revision": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip(),
                "dirty": bool(subprocess.check_output(["git", "status", "--porcelain"], cwd=ROOT)),
                "catalog_sha256": hashlib.sha256((out / "catalog.json").read_bytes()).hexdigest(),
                "simulation": "in-memory behavior simulator; no physics or hardware",
                "timeout_seconds": args.timeout_seconds}
    if args.mode == "model" and not args.prepare_only:
        metadata.update(model_metadata(args.model_url, args.model))
        if args.expected_digest and metadata["model"]["digest"] != args.expected_digest:
            raise RuntimeError("installed model digest does not match --expected-digest")
    (out / "run.json").write_text(json.dumps(metadata, indent=2) + "\n")
    if args.prepare_only:
        return 0
    if args.cases:
        env["WENDY_GO2_EVAL_CASES"] = args.cases
    env.update(WENDY_GO2_EVAL_URL=args.model_url,
               WENDY_GO2_EVAL_MODEL=args.model, WENDY_GO2_EVAL_TIMEOUT_SECONDS=str(args.timeout_seconds))
    failed = False
    summaries = []
    for attempt in range(1, args.repetitions + 1):
        report = out / f"attempt-{attempt}"
        report.mkdir()
        env["WENDY_GO2_EVAL_REPORT_DIR"] = str(report)
        test = "TestGo2EvalModel" if args.mode == "model" else "TestGo2EvalScripted"
        with (report / "test.log").open("w") as log:
            with subprocess.Popen([str(out / "chat.test"), f"-test.run=^{test}$",
                                   "-test.v", "-test.timeout=50m"], cwd=out, env=env,
                                  stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True) as process:
                for line in process.stdout:
                    log.write(line)
                    print(line, end="", flush=True)
                failed |= process.wait() != 0
        summary = summarize_reports(report, args.mode, expected)
        summaries.append(summary)
        failed |= summary["failed"] != 0
        (out / "summary.json").write_text(json.dumps({"mode": args.mode, "attempts": summaries}, indent=2) + "\n")
    return int(failed)


if __name__ == "__main__":
    raise SystemExit(main())
