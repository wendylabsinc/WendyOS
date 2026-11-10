#!/usr/bin/env python3
"""Fake `wendy` for tests: `mcp serve` speaks newline-delimited JSON-RPC; `--version` prints a version;
`chat` is a fake Wendy chat that calls two tools through its MCP server."""
import json
import os
import sys
import time

TOOLS = [{"name": name, "description": f"Fake {name}.",
          "inputSchema": {"type": "object", "properties": {}},
          "annotations": {"readOnlyHint": read_only}}
         for name, read_only in [("read_thing", True), ("write_thing", False),
                                 ("container_exec", False), ("wendy_tools", False)]]


def reply(id, result):
    sys.stdout.write(json.dumps({"jsonrpc": "2.0", "id": id, "result": result}) + "\n")
    sys.stdout.flush()


def handle(message):
    method, id, params = message.get("method"), message.get("id"), message.get("params") or {}
    if id is None:
        return
    if method == "initialize":
        reply(id, {"protocolVersion": params.get("protocolVersion", "2025-06-18"),
                   "capabilities": {"tools": {}}, "serverInfo": {"name": "fake-wendy", "version": "0.0.0"}})
    elif method == "tools/list":
        if os.environ.get("FAKE_MCP_PAGES") == "2":
            half = len(TOOLS) // 2
            if params.get("cursor") == "page2":
                reply(id, {"tools": TOOLS[half:]})
            else:
                reply(id, {"tools": TOOLS[:half], "nextCursor": "page2"})
        else:
            reply(id, {"tools": TOOLS})
    elif method == "tools/call":
        text = json.dumps({"tool": params.get("name"), "arguments": params.get("arguments")})
        failing = params.get("name") in (os.environ.get("FAKE_MCP_ERROR_TOOLS") or "").split(",")
        reply(id, {"content": [{"type": "text", "text": text}], "isError": failing})
    else:
        sys.stdout.write(json.dumps({"jsonrpc": "2.0", "id": id,
                                     "error": {"code": -32601, "message": "method not found"}}) + "\n")
        sys.stdout.flush()


def chat():
    """Like the real chat, start `mcp serve` with the command WENDY_CHAT_MCP_COMMAND names, unless
    FAKE_CHAT_IGNORES_HOOK makes this a chat built before that hook, which runs its own binary."""
    from mcp_client import StdioMCP
    command = os.environ.get("WENDY_CHAT_MCP_COMMAND")
    if not command or os.environ.get("FAKE_CHAT_IGNORES_HOOK"):
        command = os.path.abspath(sys.argv[0])
    with StdioMCP([command, "mcp", "serve", "--tool-groups", "all"], timeout=20) as mcp:
        mcp.initialize()
        for i, (name, arguments) in enumerate((("write_thing", {"path": "x"}), ("read_thing", {}))):
            print(json.dumps({"type": "tool_start", "id": str(i), "tool": name, "arguments": arguments}), flush=True)
            mcp.call(name, arguments)
    print(json.dumps({"type": "usage", "usage": {"input_tokens": 10, "output_tokens": 2, "complete": True}}))
    print(json.dumps({"type": "done", "text": "done"}))
    return 0


def main():
    if os.environ.get("FAKE_MCP_ENV_LOG"):
        state = os.environ.get("WENDY_CONFIG_DIR")
        with open(os.environ["FAKE_MCP_ENV_LOG"], "a") as f:
            f.write(json.dumps({"argv": sys.argv[1:], "config_dir": state, "exists": bool(state) and os.path.isdir(state),
                                "secret_store": os.environ.get("WENDY_SECRET_STORE")}) + "\n")
    if "--version" in sys.argv[1:]:
        print("fake-wendy 0.0.0")
        return 0
    if sys.argv[1:2] == ["chat"]:
        return chat()
    if os.environ.get("FAKE_MCP_STDERR"):
        sys.stderr.write(os.environ["FAKE_MCP_STDERR"])
        sys.stderr.flush()
    if os.environ.get("FAKE_MCP_EXIT"):
        return int(os.environ["FAKE_MCP_EXIT"])  # a server that fails at startup
    log = os.environ.get("FAKE_MCP_LOG")
    for line in sys.stdin:
        if not line.strip():
            continue
        if log:
            with open(log, "a") as f:
                f.write(line.strip() + "\n")
        if os.environ.get("FAKE_MCP_SILENT") == "1":
            continue
        handle(json.loads(line))
    return 0


if __name__ == "__main__":
    sys.exit(main())
