#!/usr/bin/env python3
"""Generate physical Linux media acceptance instructions; never contact hardware.

Inventory fields: cli_binary, agent_binary, agent_source_commit; target containing
selector, serial, owner, board, kernel and encoder_driver (bcm2835-codec or
qcom-iris); camera containing stable_id, node and physical_label; microphone
containing source_id, backend (alsa or pipewire) and physical_label; records
containing device_info, installed_binary, camera_list, sources, udev, recovery.
Each record is a path to a nonempty operator-captured file. These are provenance
inputs, not a claim that hardware acceptance has passed. Generate separate plans
for both encoder drivers and both audio backends. Use a reserved test board.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shlex
import uuid

ROOT = Path(__file__).resolve().parents[3]
CASES = ("camera-native-busy", "camera-owner-sharing", "camera-final-release",
         "encoder-negotiation", "microphone-before", "microphone-renumber",
         "microphone-after", "microphone-absent", "cleanup")


def line(value):
    if not isinstance(value, str) or not value.strip() or any(ord(c) < 32 for c in value):
        raise ValueError("inventory values must be nonempty single lines")
    return value


def artifact(value):
    path = Path(line(str(value))).resolve(strict=True)
    if not path.is_file() or not path.stat().st_size:
        raise ValueError("evidence must be a nonempty regular file")
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return {"path": str(path), "sha256": digest.hexdigest()}


def selected_source(document, source):
    matches = [s for s in document["sources"] if s.get("id") == source]
    if len(matches) != 1 or matches[0].get("kind") != "audio" or matches[0].get("healthy") is not True:
        raise ValueError("need one healthy exact microphone source")
    return matches[0]


def check_renumber(before, after, source, backend):
    a, b = selected_source(before, source), selected_source(after, source)
    domain = {"alsa": "ALSA_CAPTURE/AGENT_RECEIPT", "pipewire": "PIPEWIRE_CAPTURE/AGENT_RECEIPT"}[backend]
    # Data source listings use protojson snake_case (encodeProtoJSON).
    for row in (a, b):
        if row.get("clock_domain") != domain:
            raise ValueError("source did not use the requested physical backend")
    def numeric(row):
        aliases = row.get("aliases", [])
        if len(aliases) != 1 or not re.fullmatch(r"audio:[0-9]+", aliases[0]):
            raise ValueError("need one actual numeric alias for each observation")
        return aliases[0]
    if numeric(a) == numeric(b):
        raise ValueError("no observed numeric renumbering; repeat the physical experiment")
    return {"source_id": source, "backend": backend, "before_alias": numeric(a),
            "after_alias": numeric(b), "physical_identity_and_pcm": "requires_human_review"}


def generate(inventory, output):
    raw = json.loads(Path(inventory).read_text())
    cli, agent = artifact(raw["cli_binary"]), artifact(raw["agent_binary"])
    if not os.access(cli["path"], os.X_OK):
        raise ValueError("CLI must be executable")
    if not re.fullmatch(r"[0-9a-f]{40}", raw["agent_source_commit"]):
        raise ValueError("pin the full candidate source SHA")
    target, camera, mic = raw["target"], raw["camera"], raw["microphone"]
    for group, keys in ((target, ("selector", "serial", "owner", "board", "kernel")),
                        (camera, ("stable_id", "node", "physical_label")),
                        (mic, ("source_id", "physical_label"))):
        for key in keys:
            line(group[key])
    if target["selector"].startswith("-") or target["encoder_driver"] not in ("bcm2835-codec", "qcom-iris"):
        raise ValueError("use an explicit target and one of the two affected encoder drivers")
    if not re.fullmatch(r"/dev/video[0-9]+", camera["node"]) or not camera["stable_id"].startswith(("by-id:", "by-path:")):
        raise ValueError("camera needs its exact native node and observed stable identity")
    if mic["backend"] not in ("alsa", "pipewire") or not mic["source_id"].startswith("audio:") or re.fullmatch(r"audio:[0-9]+", mic["source_id"]):
        raise ValueError("select a stable microphone source and the actual backend")
    records = {key: artifact(raw["records"][key]) for key in
               ("device_info", "installed_binary", "camera_list", "sources", "udev", "recovery")}
    listing = json.loads(Path(records["camera_list"]["path"]).read_text())
    if len([c for c in listing if c.get("stable_id") == camera["stable_id"]]) != 1:
        raise ValueError("camera stable ID must be unique in the actual CLI listing")
    sources = json.loads(Path(records["sources"]["path"]).read_text())
    selected_source(sources, mic["source_id"])
    out = Path(output).resolve()
    out.mkdir(mode=0o700, parents=True, exist_ok=False)
    nonce = "linux-media-" + uuid.uuid4().hex[:12]
    base = [cli["path"], "--device", target["selector"]]
    command = lambda *args: base + list(args)
    commands = {
        "info": command("--json", "device", "info"),
        "cameras": command("--json", "device", "camera", "list"),
        "sources": command("--json", "data", "sources"),
        "episodes": command("--json", "data", "episodes"),
        "microphone_before": command("--json", "data", "record", "--name", nonce + "-before", "--source", mic["source_id"]),
        "microphone_after": command("--json", "data", "record", "--name", nonce + "-after", "--source", mic["source_id"]),
        "stop_owned_episode_only": command("--json", "data", "stop"),
    }
    guardian = ROOT / ".github/scripts/run-owned-media-test.py"
    view = command("device", "camera", "view", "--stable-id", camera["stable_id"], "--stdout", "--non-interactive")
    # Output is opaque media. Do not merge stderr into it or wrap with --json.
    capture = "#!/bin/sh\nset -eu\numask 077\ncd " + shlex.quote(str(out)) + "\n"
    capture += '''case "${1-}" in baseline|busy|shared-a|shared-b|released|encoder) phase=$1;; *) echo 'Choose a named procedure phase' >&2; exit 2;; esac
case "$phase" in shared-a) seconds=40;; shared-b) seconds=80;; *) seconds=45;; esac
set -C
code=0
'''
    capture += "# Inventory values are intentionally shell-quoted literals.\n# shellcheck disable=SC2016\n"
    capture += shlex.join(["python3", str(guardian), "--timeout"]) + ' "$seconds" -- ' + shlex.join(view)
    capture += ' > "$phase.video" 2> "$phase.stderr" || code=$?\nprintf "%s\\n" "$code" > "$phase.exit"\nexit "$code"\n'
    console = "#!/bin/sh\nset -eu\numask 077\n"
    console += "date -u\nuname -a\ncat /etc/os-release\ncat /proc/sys/kernel/random/boot_id\n"
    console += "v4l2-ctl --device=" + shlex.quote(camera["node"]) + " --all --list-formats-ext\n"
    console += "udevadm info --query=all --name=" + shlex.quote(camera["node"]) + "\n"
    console += '''cat /proc/asound/cards
arecord -l
for card in /proc/asound/card*/id; do test ! -f "$card" || { printf '%s: ' "$card"; cat "$card"; }; done
gst-inspect-1.0 v4l2h264enc
systemctl show wendy-agent --property=MainPID --property=ExecMainStartTimestamp --property=ExecStart
'''
    for name, value in (("capture-camera.sh", capture), ("console-capture.sh", console),
                        ("commands.json", json.dumps(commands, indent=2) + "\n")):
        (out / name).write_text(value)
        (out / name).chmod(0o600)
    guide = Path(__file__).with_suffix(".txt").read_text()
    guide = guide.replace("@PLAN@", shlex.quote(str(out))).replace("@NONCE@", nonce)
    guide += "\nExact CLI commands for this board. Run each separately at its described stage.\n"
    guide += "\n".join(name + ":\n" + shlex.join(argv) for name, argv in commands.items()) + "\n"
    guide += "\nFor each sealed owned episode, replace EPISODE with its returned ID:\n"
    guide += shlex.join(command("--json", "data", "inspect")) + ' "$EPISODE"\n'
    guide += shlex.join(command("--json", "data", "download")) + ' "$EPISODE" --output "$NEW_EPISODE_DIRECTORY"\n'
    (out / "procedure.txt").write_text(guide)
    source_paths = [Path(__file__), Path(__file__).with_suffix(".txt"), guardian,
                    ROOT / "go/internal/agent/services/video_service.go",
                    ROOT / "go/internal/agent/services/camera_shared_http.go",
                    ROOT / "go/internal/agent/services/data_audio_adapter.go",
                    ROOT / "go/internal/cli/commands/camera.go", ROOT / "go/internal/cli/commands/data.go"]
    plan = {"audit_entries": ["1739", "1954", "2542"], "nonce": nonce, "inventory": raw,
            "artifacts": {"cli": cli, "agent": agent, **records},
            "source_files": [artifact(path) for path in source_paths],
            "hardware_execution": "not_run", "cases": [{"id": case, "status": "not_run",
                "observations": [], "evidence": [], "reviewer": None} for case in CASES]}
    (out / "evidence.json").write_text(json.dumps(plan, indent=2) + "\n")
    return plan


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="action", required=True)
    build = sub.add_parser("generate")
    build.add_argument("inventory", type=Path)
    build.add_argument("output", type=Path)
    compare = sub.add_parser("check-renumber")
    compare.add_argument("before", type=Path)
    compare.add_argument("after", type=Path)
    compare.add_argument("--source", required=True)
    compare.add_argument("--backend", choices=("alsa", "pipewire"), required=True)
    args = parser.parse_args()
    if args.action == "generate":
        generate(args.inventory, args.output)
    else:
        print(json.dumps(check_renumber(json.loads(args.before.read_text()), json.loads(args.after.read_text()), args.source, args.backend), indent=2))


if __name__ == "__main__":
    main()
