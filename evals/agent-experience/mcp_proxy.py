#!/usr/bin/env python3
"""Recording MCP proxy for short tool-choice scenarios, started in place of `wendy mcp serve`.

Lines pass verbatim between the agent and a real `wendy mcp serve` child, except a tools/call the
scenario policy cans: the proxy answers it with a captured reply and the call never reaches the
child. Canned calls never return an error (spec D7). Every tools/call is appended to proxy_log.

The child is a Go server, and Go's encoding/json matches object keys case-insensitively, keeping
the last duplicate. A message Go could read differently from Python is never forwarded.
"""
import json
import os
from pathlib import Path
import subprocess
import sys
import threading
import time

import grading
from canned import FALLBACK_RESULT, CannedStore

ALWAYS_REAL = frozenset({"wendy_tools", "wendy_status", "device_list", "device_connect", "device_disconnect"})
_MESSAGE_FIELDS = frozenset({"jsonrpc", "id", "method", "params"})
_CALL_FIELDS = frozenset({"name", "arguments"})
_INVALID_REQUEST = {"code": -32600, "message": "Invalid Request"}
_PARSE_ERROR = {"code": -32700, "message": "Parse error"}
REPLY_TEXT_LIMIT = 2000


class CannedPolicy:
    def __init__(self, tools, canned, allow_real, block, passthrough=False):
        # Passthrough forwards every call except a block match: the agent runs on the device under test.
        self.passthrough = passthrough
        self.tools = {t["name"]: t for t in tools if isinstance(t, dict) and isinstance(t.get("name"), str)}
        self.canned, self.allow_real = set(canned), set(allow_real)
        # The proxy can't tell which agent is calling, so a block applies to every agent.
        self.block = [{k: v for k, v in m.items() if k != "agents"} for m in block]

    def should_can(self, name, arguments):
        entry = {"channel": "mcp", "name": name, "arguments": arguments, "source": "proxy",
                 "canned": False, "captured": None}
        if any(grading.matches(m, entry) for m in self.block):
            return True
        if self.passthrough:
            return False
        if not isinstance(name, str) or name in self.canned:
            return True
        if name in self.allow_real or name in ALWAYS_REAL:
            return False
        tool = self.tools.get(name)
        if tool is None:
            return True
        annotations = tool.get("annotations")
        return not (isinstance(annotations, dict) and annotations.get("readOnlyHint") is True)


def reply_text(message):
    """The text of a child's response, bounded: its content[].text joined, or a JSON-RPC error's message."""
    error = message.get("error")
    if error is not None:
        text = error.get("message") if isinstance(error, dict) else error
        return str(text)[:REPLY_TEXT_LIMIT]
    result = message.get("result")
    content = result.get("content") if isinstance(result, dict) else None
    if not isinstance(content, list):
        return ""
    texts = [c["text"] for c in content if isinstance(c, dict) and isinstance(c.get("text"), str)]
    return "\n".join(texts)[:REPLY_TEXT_LIMIT]


class _Object(dict):
    """A decoded JSON object that remembers its keys as sent, duplicates included."""


def _object(pairs):
    obj = _Object(pairs)
    obj.raw_keys = [k for k, _ in pairs]
    return obj


def _int(text):
    # Python refuses int() past 4300 digits and Go doesn't; a long number must not fail the parse.
    return int(text) if len(text) < 4000 else float(text)


def _ambiguous(obj, fields=frozenset()):
    """True when Go could read obj's keys differently: a case variant of a field, or a fold duplicate."""
    seen = set()
    for key in getattr(obj, "raw_keys", obj):
        folded = key.casefold()
        if folded in seen or (folded in fields and key != folded):
            return True
        seen.add(folded)
    return False


def _id_key(value):
    """Key a JSON-RPC id: 1 and "1" stay distinct, and an id echoed through Go's float64 still matches."""
    if isinstance(value, str):
        return ("s", value)
    if isinstance(value, (int, float)) and not isinstance(value, bool):
        try:
            return ("n", float(value))
        except OverflowError:
            pass
    return ("j", json.dumps(value, sort_keys=True))


def _lines(stream):
    """Yield lines, newline included, straight from the stream's descriptor.

    Raw reads keep the pump threads out of buffered-IO locks, so a thread still blocked on the
    agent's stdin when the child exits can't break interpreter shutdown.
    """
    try:
        fd = stream.fileno()
    except (AttributeError, OSError, ValueError):
        yield from iter(stream.readline, b"")
        return
    pending = bytearray()
    while True:
        try:
            chunk = os.read(fd, 65536)
        except OSError:
            break
        if not chunk:
            break
        start = len(pending)  # pending holds no newline before this chunk
        pending += chunk
        end = pending.rfind(b"\n", start)
        if end < 0:
            continue
        for line in bytes(pending[:end]).split(b"\n"):
            yield line + b"\n"
        del pending[:end + 1]
    if pending:
        yield bytes(pending)


def _writer(stream):
    try:
        fd = stream.fileno()
    except (AttributeError, OSError, ValueError):
        def write(data):
            stream.write(data)
            stream.flush()
        return write

    def write(data):
        view = memoryview(data)
        while view:
            view = view[os.write(fd, view):]
    return write


def _warn(text):
    print(f"mcp_proxy: {text}", file=sys.stderr, flush=True)


class _Proxy:
    def __init__(self, policy, store, log, child, stdout):
        self.policy, self.store, self.log, self.child = policy, store, log, child
        self._client_write, self._child_write = _writer(stdout), _writer(child.stdin)
        self._client_lock, self._log_lock, self._state = threading.Lock(), threading.Lock(), threading.Lock()
        self._client_gone = self._child_gone = False
        self._seq = 0
        self._pending = {}     # id key -> (seq, tool, arguments, t) for forwarded calls
        self._canned_ids = set()

    # --- output -------------------------------------------------------------------------------
    def to_client(self, data):
        with self._client_lock:
            if self._client_gone:
                return
            try:
                self._client_write(data)
            except OSError:
                self._client_gone = True

    def to_child(self, data):
        if self._child_gone:
            return
        try:
            self._child_write(data)
        except OSError:
            self._child_gone = True

    def reply(self, request_id, key, value):
        message = {"jsonrpc": "2.0", "id": request_id, key: value}
        self.to_client(json.dumps(message).encode() + b"\n")

    def record(self, seq, tool, arguments, canned, captured, is_error, t, reply_text=None):
        line = json.dumps({"seq": seq, "tool": tool, "arguments": {} if arguments is None else arguments,
                           "canned": canned, "captured": captured, "is_error": is_error, "t": t,
                           "reply_text": reply_text}) + "\n"
        with self._log_lock:
            if self.log.closed:
                return
            try:
                self.log.write(line)
                self.log.flush()
            except OSError as error:  # a lost record must not cost the agent its reply
                _warn(f"could not log {tool!r}: {error!r}")

    # --- agent -> child -----------------------------------------------------------------------
    def pump_client(self, stdin):
        for line in _lines(stdin):
            try:
                self.on_client_line(line)
            except Exception as error:  # never let one bad line stop the session
                _warn(f"dropped a client line after {error!r}")
        try:
            self.child.stdin.close()
        except OSError:
            pass

    def on_client_line(self, line):
        if not line.strip():
            return self.to_child(line)
        try:
            message = json.loads(line.decode("utf-8", "replace"), object_pairs_hook=_object, parse_int=_int)
        except ValueError:
            return self.to_child(line)  # not JSON; the child answers with a parse error
        except RecursionError:
            # Go's decoder accepts deeper nesting than some Pythons, so this could hide a call.
            return self.reply(None, "error", _PARSE_ERROR)
        if not isinstance(message, dict):
            return self.to_child(line)  # a batch or scalar; the Go server rejects both
        if _ambiguous(message, _MESSAGE_FIELDS):
            _warn("refused a message with case-variant or duplicate keys")
            if "id" in message:
                self.reply(message["id"], "error", _INVALID_REQUEST)
            return None
        method = message.get("method")
        if method == "tools/call":
            return self.on_call(line, message)
        if method == "notifications/cancelled" and self.cancels_canned(message.get("params")):
            return None
        return self.to_child(line)

    def on_call(self, line, message):
        params = message.get("params")
        params = params if isinstance(params, dict) else {}
        name, arguments = params.get("name"), params.get("arguments")
        args = arguments if isinstance(arguments, dict) else {}
        with self._state:
            self._seq += 1
            seq = self._seq
        t = time.time()
        ambiguous = (_ambiguous(params, _CALL_FIELDS) or _ambiguous(args)
                     or not isinstance(arguments, (dict, type(None))))
        try:
            canned = ambiguous or self.policy.should_can(name, args)
        except Exception as error:  # fail closed
            _warn(f"policy failed for {name!r}: {error!r}")
            canned = True
        has_id = "id" in message
        if not canned:
            if has_id:
                with self._state:
                    self._pending[_id_key(message["id"])] = (seq, name, arguments, t)
            else:
                self.record(seq, name, arguments, False, None, None, t)  # no response will come
            return self.to_child(line)
        try:
            result, captured = self.store.reply(name, args)
            if not isinstance(result, dict):
                raise ValueError("the capture's reply is not an object")
        except Exception as error:  # a broken capture must not surface as an error (D7)
            _warn(f"canned reply for {name!r} failed: {error!r}")
            result, captured = FALLBACK_RESULT, False
        self.record(seq, name, arguments, True, captured, result.get("isError") is True, t)
        if has_id:
            with self._state:
                self._canned_ids.add(_id_key(message["id"]))
            self.reply(message["id"], "result", result)
        return None

    def cancels_canned(self, params):
        if not isinstance(params, dict) or "requestId" not in params:
            return False
        with self._state:
            return _id_key(params["requestId"]) in self._canned_ids

    # --- child -> agent -----------------------------------------------------------------------
    def pump_child(self):
        for line in _lines(self.child.stdout):
            try:
                self.on_child_line(line)
            except Exception as error:
                _warn(f"could not record a child reply: {error!r}")
            self.to_client(line)

    def on_child_line(self, line):
        with self._state:
            if not self._pending:
                return
        try:
            message = json.loads(line)
        except ValueError:
            return
        if not isinstance(message, dict) or "method" in message or "id" not in message:
            return
        with self._state:
            call = self._pending.pop(_id_key(message["id"]), None)
        if call is None:
            return
        seq, name, arguments, t = call
        result = message.get("result")
        is_error = "error" in message or (isinstance(result, dict) and result.get("isError") is True)
        self.record(seq, name, arguments, False, None, is_error, t, reply_text(message))

    def finish(self):
        """Log forwarded calls the child never answered, then stop logging."""
        with self._state:
            unanswered = sorted(self._pending.values(), key=lambda call: call[0])
            self._pending.clear()
        for seq, name, arguments, t in unanswered:
            self.record(seq, name, arguments, False, None, None, t)
        with self._log_lock:
            self.log.close()


def run_proxy(real_wendy, argv, scenario, stdin, stdout):
    policy = CannedPolicy(json.loads(Path(scenario["tools"]).read_text()), scenario.get("canned") or [],
                          scenario.get("allow_real") or [], scenario.get("block") or [],
                          scenario.get("passthrough") is True)
    # After states key off canned calls in the proxy's own log and the CLI wrapper's.
    store = CannedStore(scenario["canned_dir"], scenario["canned_set"], scenario.get("context") or {},
                        proxy_log=scenario.get("proxy_log"), cli_log=scenario.get("cli_log"))
    log = open(scenario["proxy_log"], "a", encoding="utf-8")
    child = subprocess.Popen([real_wendy, *argv], stdin=subprocess.PIPE, stdout=subprocess.PIPE, bufsize=0)
    proxy = _Proxy(policy, store, log, child, stdout)
    from_child = threading.Thread(target=proxy.pump_child, daemon=True)
    from_client = threading.Thread(target=proxy.pump_client, args=(stdin,), daemon=True)
    from_child.start()
    from_client.start()
    code = child.wait()
    from_child.join(timeout=5)  # forward what the child wrote before it exited
    proxy.finish()
    return code if code >= 0 else 128 - code


def main(argv):
    path = os.environ.get("WENDY_EVAL_SCENARIO")
    if not path:
        _warn("WENDY_EVAL_SCENARIO is not set")
        return 2
    scenario = json.loads(Path(path).read_text())
    return run_proxy(scenario["real_wendy"], argv, scenario, sys.stdin.buffer, sys.stdout.buffer)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
