#!/usr/bin/env python3
"""Keep PAIR's stdio broker alive and expose its RPC API on a private socket."""

import argparse
import asyncio
from collections import deque
import contextlib
import json
import os
from pathlib import Path
import signal
import sys
import urllib.request


SOCKET = "/run/nvpair/control.sock"


class Broker:
    def __init__(self, process):
        self.process = process
        self.pending = {}
        self.next_id = 0
        self.ready = asyncio.Event()
        self.events = deque(maxlen=100)
        self.configured = False
        self.model_ready = False

    async def read(self):
        try:
            while line := await self.process.stdout.readline():
                message = json.loads(line)
                if "id" in message:
                    future = self.pending.get(message["id"])
                    if future is not None and not future.done():
                        if "error" in message:
                            future.set_exception(RuntimeError(json.dumps(message["error"])))
                        else:
                            future.set_result(message.get("result"))
                elif message.get("method") == "app:ready":
                    self.ready.set()
                elif message.get("method", "").startswith(("cluster:", "nodes:")):
                    # Invitations are available to pairctl without logging PINs.
                    self.events.append(message)
        finally:
            for future in self.pending.values():
                if not future.done():
                    future.set_exception(RuntimeError("PAIR broker disconnected"))

    async def call(self, method, params=None, timeout=60):
        if self.process.returncode is not None:
            raise RuntimeError("PAIR broker is not running")
        self.next_id += 1
        request_id = self.next_id
        future = asyncio.get_running_loop().create_future()
        self.pending[request_id] = future
        try:
            request = {"jsonrpc": "2.0", "id": request_id, "method": method}
            if params is not None:
                request["params"] = params
            self.process.stdin.write((json.dumps(request) + "\n").encode())
            await self.process.stdin.drain()
            return await asyncio.wait_for(future, timeout)
        finally:
            self.pending.pop(request_id, None)

    async def health(self):
        status = await self.call("proxy:get-status")
        if not self.configured or not status.get("ready"):
            raise RuntimeError("PAIR proxy is starting or unavailable")
        if status.get("port") != int(os.environ.get("PAIR_PORT", "11434")):
            raise RuntimeError("PAIR proxy port differs from PAIR_PORT; check for a port conflict")
        if os.environ.get("ENABLE_OLLAMA", "true").lower() == "true":
            engine = await self.call("engine:status", {"engine": "ollama"})
            if not engine.get("healthy") or not self.model_ready:
                raise RuntimeError("Ollama/model is not ready; check the app logs")
        return status

    async def prepare_engine(self):
        model = Path("/opt/ollama-models/example-model").read_text().strip()
        await self.call("engine:start", {"engine": "ollama"}, timeout=180)
        inventory = await self.call("engine:models")
        if model not in inventory.get("modelsByEngine", {}).get("ollama", []):
            raise RuntimeError(f"Packaged model {model} is missing from Ollama")
        engine = await self.call("engine:status", {"engine": "ollama"})

        def warm():
            request = urllib.request.Request(
                f"http://127.0.0.1:{engine['port']}/api/generate",
                data=json.dumps({"model": model, "prompt": "Hello", "stream": False,
                                 "options": {"num_predict": 8}, "keep_alive": -1}).encode(),
                headers={"Content-Type": "application/json"},
            )
            with urllib.request.urlopen(request, timeout=600) as response:
                result = json.load(response)
            if not result.get("done") or result.get("error"):
                raise RuntimeError(f"Model warmup failed: {result}")

        await asyncio.to_thread(warm)
        self.model_ready = True
        print(f"Ollama model {model} is ready on backend port {engine['port']}.", flush=True)

    async def configure(self):
        await asyncio.wait_for(self.ready.wait(), 120)
        port = int(os.environ.get("PAIR_PORT", "11434"))
        for _ in range(120):
            status = await self.call("proxy:get-status")
            if status.get("ready"):
                break
            await asyncio.sleep(1)
        else:
            raise RuntimeError("PAIR proxy did not start; check the worker logs")
        if status["port"] != port:
            status = await self.call("proxy:set-port", {"port": port})
        if status.get("port") != port:
            raise RuntimeError(f"PAIR could not reserve port {port}; choose a free PORT")
        if os.environ.get("ENABLE_OLLAMA", "true").lower() == "true":
            await self.prepare_engine()
        self.configured = True
        print(f"PAIR router listening locally at http://127.0.0.1:{port}", flush=True)

    async def client(self, reader, writer):
        try:
            request = json.loads(await asyncio.wait_for(reader.readline(), 10))
            method = request["method"]
            if method == "health":
                result = await self.health()
            elif method == "events":
                result = list(self.events)
            else:
                result = await self.call(method, request.get("params"), timeout=3600)
            response = {"result": result}
        except (RuntimeError, TimeoutError, OSError, ValueError, KeyError, TypeError) as error:
            response = {"error": str(error)}
        try:
            writer.write((json.dumps(response) + "\n").encode())
            await writer.drain()
        finally:
            writer.close()
            with contextlib.suppress(OSError):
                await writer.wait_closed()


async def serve():
    os.umask(0o077)
    Path(SOCKET).parent.mkdir(parents=True, exist_ok=True)
    # Fail if another wrapper owns the endpoint instead of unlinking its socket.
    if Path(SOCKET).exists():
        try:
            _, writer = await asyncio.open_unix_connection(SOCKET)
        except (ConnectionRefusedError, FileNotFoundError):
            Path(SOCKET).unlink(missing_ok=True)
        else:
            writer.close()
            await writer.wait_closed()
            raise RuntimeError("Another PAIR wrapper is already running")
    process = await asyncio.create_subprocess_exec(
        "./nvpair-ui-broker", stdin=asyncio.subprocess.PIPE,
        stdout=asyncio.subprocess.PIPE, limit=4 * 1024 * 1024, start_new_session=True,
    )
    broker = Broker(process)
    reader_task = asyncio.create_task(broker.read())
    stop = asyncio.Event()
    loop = asyncio.get_running_loop()
    for sig in (signal.SIGINT, signal.SIGTERM):
        loop.add_signal_handler(sig, stop.set)
    stop_task = asyncio.create_task(stop.wait())
    configure_task = asyncio.create_task(broker.configure())
    tasks = [reader_task, stop_task, configure_task]
    server = None
    try:
        server = await asyncio.start_unix_server(broker.client, SOCKET)
        os.chmod(SOCKET, 0o600)
        done, _ = await asyncio.wait(tasks, return_when=asyncio.FIRST_COMPLETED)
        if stop_task in done:
            return
        if reader_task in done:
            await reader_task
            raise RuntimeError("PAIR broker exited unexpectedly")
        await configure_task
        done, _ = await asyncio.wait([reader_task, stop_task], return_when=asyncio.FIRST_COMPLETED)
        if reader_task in done:
            await reader_task
            raise RuntimeError("PAIR broker exited unexpectedly")
    finally:
        if server is not None:
            server.close()
            await server.wait_closed()
            Path(SOCKET).unlink(missing_ok=True)
        # SIGTERM reaches the broker, which gracefully stops its owned workers
        # and engines. Keep draining stdout while it shuts down.
        if process.returncode is None:
            process.terminate()
            try:
                await asyncio.wait_for(process.wait(), 20)
            except TimeoutError:
                os.killpg(process.pid, signal.SIGKILL)
                await process.wait()
        for task in tasks:
            task.cancel()
        await asyncio.gather(*tasks, return_exceptions=True)


async def request(method, params):
    reader, writer = await asyncio.open_unix_connection(SOCKET, limit=4 * 1024 * 1024)
    try:
        writer.write((json.dumps({"method": method, "params": params}) + "\n").encode())
        await writer.drain()
        response = json.loads(await asyncio.wait_for(reader.readline(), 3610))
        if "error" in response:
            raise RuntimeError(response["error"])
        print(json.dumps(response["result"], indent=2))
    finally:
        writer.close()
        await writer.wait_closed()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    commands.add_parser("serve", help="run the PAIR broker (container entrypoint)")
    commands.add_parser("health", help="check local proxy, engine, and model readiness")
    commands.add_parser("events", help="show recent cluster events, including incoming invitations")
    rpc = commands.add_parser("rpc", help="call a PAIR broker JSON-RPC method")
    rpc.add_argument("method")
    rpc.add_argument("params", nargs="?", default="{}", help="JSON object")
    args = parser.parse_args()
    try:
        if args.command == "serve":
            asyncio.run(serve())
        else:
            params = json.loads(args.params) if args.command == "rpc" else None
            if args.command == "rpc" and not isinstance(params, dict):
                parser.error("params must be a JSON object")
            asyncio.run(request(args.method if args.command == "rpc" else args.command, params))
    except (RuntimeError, OSError, ValueError, TimeoutError) as error:
        print(f"PAIR: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
