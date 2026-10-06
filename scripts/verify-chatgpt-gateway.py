#!/usr/bin/env python3
"""Exercise the actual stdio gateway. Camera/app writes require explicit flags."""

import argparse
import base64
import json
import os
from pathlib import Path
import selectors
import subprocess
import time
import http.cookiejar
import urllib.request


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--wendy", default="wendy")
    parser.add_argument("--config", required=True)
    parser.add_argument("--robot")
    parser.add_argument("--mentions-only", action="store_true", help="Verify composer mentions and resource reads without connecting to a device")
    parser.add_argument("--camera", type=int)
    parser.add_argument("--image-out", type=Path)
    parser.add_argument("--cycle-app", help="Stop then start only this permitted test app")
    parser.add_argument("--call", action="append", default=[], help='JSON {"name":"export_name","arguments":{...}}')
    parser.add_argument("--tool", action="append", default=[], help='Exact gateway tool call JSON, with top-level arguments')
    parser.add_argument("--preview-camera", type=int, help="Start a continuous preview, verify multiple frames, then stop")
    parser.add_argument("--open-app", help="Verify the declared app web UI without changing app state")
    args = parser.parse_args()
    if not args.robot and not args.mentions_only:
        parser.error("--robot is required unless --mentions-only is set")
    if (args.camera is None) != (args.image_out is None):
        parser.error("--camera and --image-out must be supplied together")
    proc = subprocess.Popen([args.wendy, "mcp", "gateway", "--config", args.config],
                            stdin=subprocess.PIPE, stdout=subprocess.PIPE)
    selector = selectors.DefaultSelector()
    selector.register(proc.stdout, selectors.EVENT_READ)
    buffer = b""
    next_id = 0

    def request(method, params):
        nonlocal buffer, next_id
        next_id += 1
        proc.stdin.write((json.dumps(dict(jsonrpc="2.0", id=next_id, method=method, params=params)) + "\n").encode())
        proc.stdin.flush()
        deadline = time.monotonic() + 60
        while True:
            while b"\n" in buffer:
                line, buffer = buffer.split(b"\n", 1)
                message = json.loads(line)
                if message.get("id") != next_id:
                    continue
                if "error" in message:
                    raise RuntimeError(message["error"])
                return message["result"]
            remaining = deadline - time.monotonic()
            if remaining <= 0 or not selector.select(remaining):
                raise TimeoutError("Gateway request timed out; do not automatically repeat a write")
            chunk = os.read(proc.stdout.fileno(), 65536)
            if not chunk:
                raise RuntimeError("Gateway exited before replying")
            buffer += chunk
            if len(buffer) > 8 * 1024 * 1024:
                raise RuntimeError("Gateway response exceeded test limit")

    def call(name, arguments):
        result = request("tools/call", dict(name=name, arguments=arguments))
        if result.get("isError"):
            raise RuntimeError(result.get("content"))
        print(json.dumps({"tool": name, "result": result.get("structuredContent", [c for c in result.get("content", []) if c["type"] == "text"]) }), flush=True)
        return result

    try:
        init = request("initialize", {"protocolVersion": "2025-11-25", "capabilities": {}, "clientInfo": {"name": "wendy-gateway-check", "version": "1"}})
        proc.stdin.write(b'{"jsonrpc":"2.0","method":"notifications/initialized"}\n')
        proc.stdin.flush()
        catalog = request("tools/list", {})
        mentions = next(t for t in catalog["tools"] if t["name"] == "search_devices")
        assert mentions["_meta"]["openai/extensions"]["mentions/search"] == {}
        assert "app" in mentions["_meta"]["ui"]["visibility"]
        assert "query" in mentions["inputSchema"]["required"]
        assert "items" in mentions["outputSchema"]["required"]
        found = request("tools/call", {"name": "search_devices", "arguments": {"query": ""}})
        assert not found.get("isError"), found.get("content")
        assert found["content"] == []
        assert set(found["structuredContent"]) == {"items"}
        items = found["structuredContent"]["items"]
        assert isinstance(items, list) and len(items) <= 100
        assert all(i["type"] == "resource_link" and i["uri"].startswith("wendy://devices/") and i["name"] for i in items)
        if items:
            selected = items[0]
            resource = request("resources/read", {"uri": selected["uri"]})["contents"][0]
            assert resource["uri"] == selected["uri"] and resource["mimeType"] == "application/json"
            identity = json.loads(resource["text"])
            assert identity["robot_id"] == selected["uri"].removeprefix("wendy://devices/")
            assert identity["connection"] == "unknown"
            narrowed = request("tools/call", {"name": "search_devices", "arguments": {"query": identity["robot_id"]}})
            assert any(i["uri"] == selected["uri"] for i in narrowed["structuredContent"]["items"])
        print(json.dumps({"mentions": "verified", "returned": len(items), "resource_read": "verified" if items else "no devices", "discovery_complete": found.get("_meta", {}).get("discovery_complete")}), flush=True)
        if args.mentions_only:
            return
        settings = init.get("capabilities", {}).get("experimental", {}).get("openai/settings")
        if settings:
            names = {t["name"] for t in catalog["tools"]}
            assert {settings["readTool"], settings["updateTool"]} <= names
        panel = next(t for t in catalog["tools"] if t["name"] == "open_robot")
        assert {e["type"] for e in panel["_meta"]["openai/ui"]["entrypoints"]} == {"thread"}
        fleet = next(t for t in catalog["tools"] if t["name"] == "open_devices")
        assert {e["type"] for e in fleet["_meta"]["openai/ui"]["entrypoints"]} == {"global"}
        resource = request("resources/read", {"uri": panel["_meta"]["ui"]["resourceUri"]})
        assert "ui/initialize" in resource["contents"][0]["text"]
        print(json.dumps({"tools": [t["name"] for t in catalog["tools"]], "panel_resource": "verified"}), flush=True)
        call("list_robots", {})
        call("inspect_robot", {"robot_id": args.robot})
        if args.cycle_app:
            for action in ("stop", "start"):
                result = call(action + "_robot_app", {"robot_id": args.robot, "app_name": args.cycle_app})
                assert result["structuredContent"]["state_verified"] is True
        for raw in args.call:
            operation = json.loads(raw)
            call(operation["name"], {"robot_id": args.robot, "arguments": operation.get("arguments", {})})
        for raw in args.tool:
            operation = json.loads(raw)
            call(operation["name"], operation.get("arguments", {}))
        if args.open_app:
            opened = request("tools/call", {"name": "open_robot_app", "arguments": {"robot_id": args.robot, "app_name": args.open_app}})
            if opened.get("isError"):
                raise RuntimeError(opened.get("content"))
            opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))
            with opener.open(opened["structuredContent"]["url"], timeout=20) as response:
                page = response.read(2 * 1024 * 1024)
                assert response.status == 200 and b"html" in page[:1000].lower()
                print(json.dumps({"app_web_ui": args.open_app, "http_status": response.status, "html_bytes": len(page)}), flush=True)
        if args.preview_camera is not None:
            started = call("start_camera_preview", {"robot_id": args.robot, "camera_id": args.preview_camera})
            preview = {"robot_id": args.robot, "preview_id": started["structuredContent"]["preview_id"]}
            try:
                sequences = []
                for _ in range(3):
                    frame = call("read_camera_preview", preview)
                    assert len(base64.b64decode(frame["_meta"]["frame"]["data"], validate=True)) <= 2 * 1024 * 1024
                    assert all(c["type"] != "image" for c in frame["content"])
                    sequences.append(frame["structuredContent"]["sequence"])
                    time.sleep(0.6)
                assert sequences[-1] > sequences[0], "Preview did not advance"
            finally:
                call("stop_camera_preview", preview)
        if args.camera is not None:
            result = call("capture_robot_image", {"robot_id": args.robot, "camera_id": args.camera})
            image = next(c for c in result["content"] if c["type"] == "image")
            assert image["mimeType"] == "image/jpeg"
            data = base64.b64decode(image["data"], validate=True)
            assert data.startswith(b"\xff\xd8") and len(data) <= 2 * 1024 * 1024
            # A captured frame may contain private information. Do not overwrite
            # an existing file or give other local accounts read access.
            fd = os.open(args.image_out, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
            with os.fdopen(fd, "wb") as output:
                output.write(data)
            print(json.dumps({"image_saved": str(args.image_out), "bytes": len(data)}), flush=True)
    finally:
        selector.close()
        proc.stdin.close()
        try:
            proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            proc.terminate()
            try:
                proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                proc.kill()
                proc.wait()


if __name__ == "__main__":
    main()
