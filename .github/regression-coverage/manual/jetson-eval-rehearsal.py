#!/usr/bin/env python3
"""Prepare audit2414's reserved-Thor trial. No deployment or model download."""
import argparse
import hashlib
import ipaddress
import json
from pathlib import Path
import re
import shlex
import shutil
import subprocess
import uuid

HERE = Path(__file__).resolve().parent
MODEL = "qwen3-coder-30b-a3b"


def line(value):
    if not isinstance(value, str) or not value or any(ord(c) < 32 for c in value):
        raise ValueError("require nonempty single-line identity")
    return value


def sha(value):
    if not isinstance(value, str) or not re.fullmatch(r"[0-9a-f]{64}", value):
        raise ValueError("require full SHA256")
    return value


def artifact(path, allow_empty=False):
    path = Path(path).resolve(strict=True)
    if not path.is_file() or not (0 if allow_empty else 1) <= path.stat().st_size <= 1024**3:
        raise ValueError("require regular nonempty artifact no larger than 1 GiB")
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(1024**2), b""):
            digest.update(chunk)
    return {"path": str(path), "sha256": digest.hexdigest(), "bytes": path.stat().st_size}


def source_identity(root, expected):
    if not re.fullmatch(r"[0-9a-f]{40}", expected):
        raise ValueError("require full source commit")
    result = subprocess.run(["git", "-C", str(root), "rev-parse", "HEAD"],
                            capture_output=True, text=True, timeout=10, check=True)
    if result.stdout.strip() != expected:
        raise ValueError("source checkout differs from admitted commit")
    result = subprocess.run(["git", "-C", str(root), "status", "--porcelain", "--untracked-files=all", "--",
                             "evals/agent-experience", "plugins/wendy-agentic-coding/skills"],
                            capture_output=True, text=True, timeout=10, check=True)
    if result.stdout:
        raise ValueError("require clean tracked harness and skill inputs")


def generate(inventory, output):
    data = json.loads(Path(inventory).read_text())
    def no_secrets(value):
        if isinstance(value, dict):
            for name, item in value.items():
                if any(word in name.lower() for word in ("password", "secret", "token", "private_key")):
                    raise ValueError("credentials must not enter inventory")
                no_secrets(item)
        elif isinstance(value, list):
            for item in value:
                no_secrets(item)
    no_secrets(data)
    if not re.fullmatch(r"[0-9a-f]{32}", data["trial"]):
        raise ValueError("require a fresh 128-bit trial nonce")
    board = data["board"]
    if board["device_type"] != "jetson-agx-thor" or board["soc"] != "tegra264":
        raise ValueError("this procedure qualifies a reserved physical Thor")
    for name in ("serial", "device_tree_serial", "ecid", "root_disk_serial", "boot_id", "owner", "os_build", "firmware", "address"):
        line(board[name])
    host, port = board["address"].rsplit(":", 1)
    address = ipaddress.ip_address(host.strip("[]"))
    if address.is_unspecified or address.is_multicast or address.is_loopback or not 1 <= int(port) <= 65535:
        raise ValueError("require literal direct physical target endpoint")
    if str(uuid.UUID(board["boot_id"])) != board["boot_id"]:
        raise ValueError("require exact Linux boot UUID")
    if data.get("reserved_disposable") is not True or data.get("empty_apps_and_volumes") is not True:
        raise ValueError("require owner-reserved empty disposable device")
    for name in ("agent_sha256", "staged_cli_sha256", "host_cli_sha256"):
        sha(data[name])
    records = {name: artifact(data["records"][name]) for name in
               ("identity", "authorization", "recovery", "agent_build", "cli_build", "resource_budget")}
    root = Path(data["source_root"]).resolve(strict=True)
    source_identity(root, data["source_commit"])
    binaries = {name: artifact(data[name]) for name in ("host_cli", "staged_cli")}
    for name, record in binaries.items():
        if record["sha256"] != data[name + "_sha256"]:
            raise ValueError(name + " hash differs")
    harness = root / "evals/agent-experience"
    model = json.loads((harness / "models.json").read_text())[MODEL]
    for item in model["files"]:
        sha(item["sha256"])
    if model["revision"] != "b17cb02dd882d5b6ab62fc777ad2995f19668350" or sum(f["bytes"] for f in model["files"]) != 18556689568:
        raise ValueError("model pin changed; requalify this procedure")
    output = Path(output).resolve()
    output.mkdir(mode=0o700, parents=True, exist_ok=False)
    project = output / "project"
    shutil.copytree(harness, project, ignore=shutil.ignore_patterns(".stage", "__pycache__", "*.pyc", "results"))
    staged = project / ".stage"
    skills = staged / "treatment/plugins/wendy-agentic-coding/skills"
    shutil.copytree(root / "plugins/wendy-agentic-coding/skills", skills)
    (staged / "treatment/REVISION").write_text(data["source_commit"] + "\n")
    shutil.copyfile(binaries["staged_cli"]["path"], staged / "wendy-linux-arm64")
    (staged / "wendy-linux-arm64").chmod(0o755)
    app = "dev.wendy.eval.rehearsal." + data["trial"]
    config = json.loads((project / "wendy.json").read_text())
    config["appId"] = app
    volumes = []
    for entitlement in config["entitlements"]:
        if entitlement["type"] == "persist":
            entitlement["name"] = "eval-" + data["trial"] + "-" + entitlement["path"].strip("/").replace("/", "-")
            volumes.append(entitlement["name"])
    (project / "wendy.json").write_text(json.dumps(config, indent=2) + "\n")
    config = json.loads((project / "config.on-device.json").read_text())
    config["max_agent_seconds"] = 120
    config["environment_context"] = f"You are inside the benchmark app {app} on this disposable device. Leave that app alone."
    (project / "config.on-device.json").write_text(json.dumps(config, indent=2) + "\n")
    shutil.copyfile(HERE / "jetson-eval-controls.py", project / "rehearsal-controls.py")
    plan = {"audit": "2414", "hardware_execution": "not_run", "trial": data["trial"], "board": board,
            "source_commit": data["source_commit"], "binaries": binaries, "agent_sha256": data["agent_sha256"],
            "records": records, "app_id": app, "volumes": volumes, "model_id": MODEL, "model": model,
            "bounds": {"deployment_seconds": 1800, "pass_seconds": 3600, "control_seconds": 240,
                       "max_agent_seconds": 120, "attempt_limit": 1},
            "cases": [{"id": name, "status": "not_run"} for name in
                      ("local-gpu-pass", "cli-and-mcp-guard", "cli-timeout-report", "stop-owned-app")],
            "sources": [artifact(path, allow_empty=True) for path in sorted(harness.rglob("*"))
                        if path.is_file() and not any(p in (".stage", "__pycache__", "results") for p in path.relative_to(harness).parts)
                        and path.suffix != ".pyc"]}
    (project / "rehearsal-plan.json").write_text(json.dumps(plan, indent=2) + "\n")
    profile = output / "cli-profile"
    profile.mkdir(mode=0o700)
    (profile / "config.json").write_text(json.dumps({"analytics": {"enabled": False},
                                                   "completionInstalled": True, "completionPromptDismissed": True}) + "\n")
    (profile / "config.json").chmod(0o600)
    prefix = ["env", "WENDY_CONFIG_DIR=" + str(profile), "WENDY_SECRET_STORE=file",
              "WENDY_ANALYTICS=false", "WENDY_TLS_SESSION_STORE=off",
              binaries["host_cli"]["path"], "--device", board["address"]]
    commands = {"info": prefix + ["--json", "device", "info", "--read-only"],
                "apps": prefix + ["--json", "device", "apps", "list"],
                "deploy": prefix + ["run", "--detach", "--skip-cloud-registration", "--env", "EVAL_MODEL=" + MODEL,
                                     "--env", "EVAL_LIMIT=1", "--env", "EVAL_REPETITIONS=1",
                                     "--env", "EVAL_PASS=" + data["trial"], "--env", "WENDY_APP_ID=" + app],
                "pass": prefix + ["device", "attach", app, "--", "cat", "/results/latest/pass.json"],
                "report": prefix + ["device", "attach", app, "--", "cat", "/results/latest/report.md"],
                "error": prefix + ["device", "attach", app, "--", "cat", "/results/last-error.txt"],
                "verify": prefix + ["device", "attach", app, "--", "timeout", "--kill-after=10s", "600s", "python3", "/opt/harness/rehearsal-controls.py", "verify"],
                "controls": prefix + ["device", "attach", app, "--", "timeout", "--kill-after=10s", "240s", "python3", "/opt/harness/rehearsal-controls.py", "controls"],
                "export": prefix + ["device", "attach", app, "--", "tar", "-C", "/results", "-cf", "-", "."],
                "stop": prefix + ["device", "apps", "stop", app],
                "remove": prefix + ["device", "apps", "remove", app, "--force"]}
    guardian = root / ".github/scripts/run-owned-media-test.py"
    plan["sources"].append(artifact(guardian))
    for name, argv in commands.items():
        seconds = {"deploy": 1800, "verify": 660, "controls": 300, "export": 300}.get(name, 45)
        commands[name] = ["python3", str(guardian), "--timeout", str(seconds), "--", *argv]
    (output / "commands.json").write_text(json.dumps(commands, indent=2) + "\n")
    (output / "commands.txt").write_text("\n".join(name + ": " + shlex.join(argv) for name, argv in commands.items()) + "\n")
    shutil.copyfile(HERE / "jetson-eval-rehearsal.txt", output / "procedure.txt")
    plan["generated"] = [artifact(path, allow_empty=True) for path in sorted(output.rglob("*")) if path.is_file()]
    (output / "evidence.json").write_text(json.dumps(plan, indent=2) + "\n")
    return plan


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("inventory", type=Path)
    parser.add_argument("output", type=Path)
    args = parser.parse_args()
    generate(args.inventory, args.output)
