#!/usr/bin/env python3
"""Verify concurrent PAIR inference and report the nodes that actually served it."""

import argparse
from collections import Counter
from concurrent.futures import ThreadPoolExecutor
import json
import os
from pathlib import Path
import socket
import time
import urllib.request


def rpc(method, params=None):
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as client:
        client.settimeout(60)
        client.connect("/run/nvpair/control.sock")
        client.sendall((json.dumps({"method": method, "params": params}) + "\n").encode())
        with client.makefile("rb") as stream:
            response = json.loads(stream.readline())
    if "error" in response:
        raise RuntimeError(response["error"])
    return response["result"]


def workload_key(workload):
    return tuple(workload.get(key) for key in ("originatedFrom", "engine", "runId", "id"))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--requests", type=int, default=4)
    parser.add_argument("--min-nodes", type=int, default=2)
    parser.add_argument("--timeout", type=int, default=180)
    args = parser.parse_args()
    if not 1 <= args.min_nodes <= args.requests <= 32 or args.timeout < 1:
        parser.error("require 1 <= min-nodes <= requests <= 32 and timeout > 0")

    rpc("health")
    model = Path("/opt/ollama-models/example-model").read_text().strip()
    origin = rpc("cluster:get-node-id")["nodeUuid"]
    selection = rpc("proxy:node/selected")
    if selection.get("id"):
        raise RuntimeError("Clear the manual proxy pin before testing automatic fleet routing")
    nodes = rpc("proxy:nodes/list")["nodes"]
    eligible = {node["id"] for node in nodes if model in node.get("models", [])}
    if len(eligible) < args.min_nodes:
        raise RuntimeError(f"Only {len(eligible)} nodes advertise {model}; need {args.min_nodes}")
    discovered = rpc("discovery:get-nodes")["nodes"]
    warmed = {node["hostUuid"] for node in discovered
              if model in node.get("loadedByEngine", {}).get("ollama", [])}
    if cold := eligible - warmed:
        raise RuntimeError(f"Model warmup is incomplete on {sorted(cold)}; wait for health on every node")
    baseline = {workload_key(w) for w in rpc("workloads:get-initial")["workloads"]}
    url = f"http://127.0.0.1:{os.environ.get('PAIR_PORT', '11434')}/api/generate"

    def generate(index):
        request = urllib.request.Request(
            url,
            data=json.dumps({
                "model": model,
                "prompt": f"Request {index}: Write a detailed explanation of how rain forms, in ten sentences.",
                "stream": False,
                "options": {"num_predict": 128, "temperature": 0.2},
            }).encode(),
            headers={"Content-Type": "application/json"},
        )
        with urllib.request.urlopen(request, timeout=args.timeout) as response:
            result = json.load(response)
        if not result.get("done") or not result.get("response", "").strip() or result.get("error"):
            raise RuntimeError(f"Inference {index} did not produce a completed text response: {result}")
        return {"request": index, "tokens": result.get("eval_count"),
                "text": result["response"][:160]}

    started = time.monotonic()
    with ThreadPoolExecutor(max_workers=args.requests) as pool:
        responses = list(pool.map(generate, range(args.requests)))
    # The terminal workload notification can arrive just after the HTTP body.
    deadline = time.monotonic() + 10
    while True:
        workloads = [w for w in rpc("workloads:get-initial")["workloads"]
                     if workload_key(w) not in baseline and w.get("originatedFrom") == origin
                     and w.get("engine") == "ollama" and w.get("model") == model]
        if len(workloads) >= args.requests and all(w.get("state") == "completed" for w in workloads):
            break
        if time.monotonic() >= deadline:
            raise RuntimeError(f"Missing completed workload evidence: {workloads}")
        time.sleep(0.2)
    if len(workloads) != args.requests:
        raise RuntimeError("Other inference traffic overlapped this check; rerun on an idle node")
    served = Counter(w.get("scheduledOn") for w in workloads)
    if not set(served).issubset(eligible) or len(served) < args.min_nodes:
        raise RuntimeError(f"Responses succeeded but fleet dispatch was not proven: {dict(served)}")
    print(json.dumps({"model": model, "requests": len(responses),
                      "elapsed_seconds": round(time.monotonic() - started, 2),
                      "served_by": dict(served), "responses": responses}, indent=2))


if __name__ == "__main__":
    main()
