#!/usr/bin/env python3
"""Capture real Wendy output (MCP tool reply or CLI run) into canned/<set>/, scrubbed."""
import argparse
import hashlib
import json
import os
import signal
import subprocess
import sys
from pathlib import Path

import shutil

from benchmark import isolated_device_state
from canned import _ACTION as ACTION, scrub, scrub_output
from mcp_client import StdioMCP
from tool_trace import cli_path

DEFAULT_DIR = Path(__file__).resolve().parent / "canned"


def device_kind(selector):
    if selector == "local":
        return "local"
    if selector.startswith("vm:"):
        return "simulator"
    if selector.startswith("cloud://"):
        return "cloud"
    return "physical"


def _version(wendy, env=None):
    out = subprocess.run([wendy, "--version"], env=env, capture_output=True, text=True, timeout=30)
    return (out.stdout or out.stderr).strip()


def _sha256(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def capture_mcp(args, canned_dir, provenance, env=None):
    # Not `mcp serve --device`: its startup connection is asynchronous, so a call made right after
    # initialize could capture NOT_CONNECTED. device_connect below waits for the connection.
    argv = [args.wendy, "mcp", "serve", "--tool-groups", "all"]
    with StdioMCP(argv, env=env, timeout=args.timeout) as mcp:
        mcp.initialize()
        tools = {t["name"]: t for t in mcp.list_tools()}
        if args.tool not in tools:
            print(f"unknown tool {args.tool}", file=sys.stderr)
            return 2
        read_only = (tools[args.tool].get("annotations") or {}).get("readOnlyHint") is True
        if not read_only and not args.yes:
            print(f"{args.tool} is not read-only; pass --yes to run it for real", file=sys.stderr)
            return 2
        if not args.no_connect:
            connected = mcp.call("device_connect", {"device": args.device})
            if connected.get("isError"):
                print(f"device_connect {args.device} failed: {json.dumps(connected)}", file=sys.stderr)
                return 1
        result = mcp.call(args.tool, json.loads(args.args or "{}"))
    if result.get("isError"):
        # Canned calls never return an error to the agent (D7), so an error reply is never saved.
        print(f"{args.tool} returned an error, nothing saved: {json.dumps(result)}", file=sys.stderr)
        return 1
    path = Path(canned_dir) / args.set / f"{args.tool}.json"
    data = {"tool": args.tool, "replies": []}
    if path.is_file():
        data = json.loads(path.read_text())
    data["captured_with"] = provenance
    if args.after:
        # The state once one of these actions was canned; stepped like replies.
        after = data.get("after") or {"actions": args.after, "replies": []}
        after["actions"] = args.after
        after["replies"].append(scrub(result))
        data["after"] = after
    else:
        data["replies"].append(scrub(result))
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(data, indent=2) + "\n")
    print(path)
    print("Replace values that came from your arguments with {{args.NAME}} placeholders.")
    return 0


def _run_cli(argv, interrupt_after=None, env=None):
    """Run argv with no stdin, as the shim does. With interrupt_after, a command that streams until
    Ctrl+C gets SIGINT after that many seconds and keeps what it printed."""
    proc = subprocess.Popen(argv, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                            text=True, env=env)
    try:
        stdout, stderr = proc.communicate(timeout=interrupt_after or 120)
    except subprocess.TimeoutExpired:
        if not interrupt_after:
            proc.kill()
            proc.communicate()
            raise
        proc.send_signal(signal.SIGINT)
        try:
            stdout, stderr = proc.communicate(timeout=15)  # retrying communicate keeps earlier output
        except subprocess.TimeoutExpired:
            proc.kill()
            stdout, stderr = proc.communicate()
    code = proc.returncode
    return (code if code >= 0 else 128 - code), stdout, stderr


def capture_cli(args, canned_dir, env=None):
    words = args.cli
    prefix = cli_path(words)
    if args.prefix_words:
        prefix = prefix[:args.prefix_words]
    if not prefix:
        print("no subcommand words found in --cli", file=sys.stderr)
        return 2
    if not args.yes:
        print("CLI commands are not classified read-only; pass --yes to run it for real", file=sys.stderr)
        return 2
    if args.after and not (Path(canned_dir) / args.set / "cli" / ("-".join(prefix) + ".json")).is_file():
        print("capture the before state of this command first, then its --after state", file=sys.stderr)
        return 2
    try:
        code, stdout, stderr = _run_cli([args.wendy, "--device", args.device, *words], args.interrupt_after, env)
    except subprocess.TimeoutExpired:
        print("the command did not exit within 120s; pass --interrupt-after for a streaming command",
              file=sys.stderr)
        return 1
    path = Path(canned_dir) / args.set / "cli" / ("-".join(prefix) + ".json")
    reply = {"stdout": scrub_output(stdout), "stderr": scrub_output(stderr), "exit_code": code}
    if args.after:
        # The before state is the capture itself, so it must exist first.
        data = json.loads(path.read_text())
        data["after"] = {"actions": args.after, **reply}
    else:
        data = {"argv_prefix": prefix, **reply}
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(data, indent=2) + "\n")
    print(path)
    return 0


def main(argv=None):
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--wendy", required=True)
    p.add_argument("--device", required=True)
    p.add_argument("--set", required=True)
    mode = p.add_mutually_exclusive_group(required=True)
    mode.add_argument("--tool")
    mode.add_argument("--cli", nargs=argparse.REMAINDER, metavar="WORD")
    p.add_argument("--args")
    p.add_argument("--yes", action="store_true")
    p.add_argument("--canned-dir", default=str(DEFAULT_DIR))
    p.add_argument("--prefix-words", type=int)
    p.add_argument("--no-connect", action="store_true",
                   help="MCP mode: skip device_connect, for host-side tools (device_connect boots a stopped VM).")
    p.add_argument("--timeout", type=float, default=120, help="MCP mode: seconds to wait for each reply.")
    p.add_argument("--interrupt-after", type=float, metavar="SECONDS",
                   help="CLI mode: send SIGINT (Ctrl+C) after this long, for a command that streams until stopped.")
    p.add_argument("--after", metavar="ACTIONS",
                   help="Record the reply as the state after these comma-separated actions (tool:NAME or "
                        "cli:WORDS) were canned, instead of the state before them.")
    # Everything after --cli is the command's, "--" included (argparse's REMAINDER stops at "--").
    argv = list(sys.argv[1:] if argv is None else argv)
    cli_words = None
    if "--cli" in argv:
        at = argv.index("--cli")
        argv, cli_words = argv[:at + 1], argv[at + 1:]
    args = p.parse_args(argv)
    if cli_words is not None:
        args.cli = cli_words
    if args.after is not None:
        args.after = [a.strip() for a in args.after.split(",") if a.strip()]
        if not args.after or not all(ACTION.match(a) for a in args.after):
            print("--after takes comma-separated tool:NAME or cli:WORDS actions", file=sys.stderr)
            return 2
    resolved = shutil.which(args.wendy)
    if resolved is None:
        print(f"cannot resolve --wendy {args.wendy}", file=sys.stderr)
        return 2
    args.wendy = resolved
    env, state = dict(os.environ), None
    if not env.get("WENDY_CONFIG_DIR"):
        # Never the developer's ~/.wendy: a throwaway state, as the runner uses for its own reads.
        state = isolated_device_state({"kind": device_kind(args.device)})
        env.update(WENDY_CONFIG_DIR=str(state), WENDY_SECRET_STORE="file")
    try:
        try:
            provenance = {"wendy_version": _version(resolved, env), "wendy_sha256": _sha256(resolved),
                          "device_kind": device_kind(args.device)}
        except (OSError, subprocess.SubprocessError) as e:
            print(f"cannot inspect wendy binary: {e}", file=sys.stderr)
            return 2
        if args.cli is not None:
            return capture_cli(args, args.canned_dir, env)
        return capture_mcp(args, args.canned_dir, provenance, env)
    finally:
        if state is not None:
            shutil.rmtree(state, ignore_errors=True)


if __name__ == "__main__":
    sys.exit(main())
