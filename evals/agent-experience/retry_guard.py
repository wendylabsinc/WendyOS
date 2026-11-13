"""Stop repeated CLI syntax failures using returned tool output, without steering."""

import json
import re


ANSI = re.compile(r"\x1b\[[0-?]*[ -/]*[@-~]")
SYNTAX_ERROR = re.compile(
    r'(?m)^\s*(?:✗\s*)?(unknown command "[^"\n]+" for "[^"\n]+"'
    r'|unknown flag: [^\s\n]+|invalid --device argument: [^\n]+)')


def unwrap_error_envelopes(text):
    """Replace each Wendy JSON error envelope line with its message.

    In JSON mode, which the CLI turns on by itself whenever it runs without a
    terminal (as it does under a coding agent), a failure is reported as one
    stderr line: {"error": {"code": ..., "message": ...}}. SYNTAX_ERROR is
    written against the message text, so match that.
    """
    lines = []
    for line in text.splitlines():
        candidate = line.strip()
        if candidate.startswith("{") and '"error"' in candidate:
            try:
                envelope = json.loads(candidate)
            except ValueError:
                envelope = None
            error = envelope.get("error") if isinstance(envelope, dict) else None
            if isinstance(error, dict) and isinstance(error.get("message"), str):
                lines.append(error["message"])
                continue
        lines.append(line)
    return "\n".join(lines)


def output_text(value):
    if isinstance(value, str):
        return value
    if isinstance(value, list):
        return "\n".join(output_text(v) for v in value)
    if isinstance(value, dict):
        return output_text(value.get("text", value.get("content", "")))
    return ""


def returned_outputs(event):
    kind = event.get("type")
    item = event.get("item", {})
    if kind == "item.completed":
        if item.get("type") == "command_execution":
            yield item.get("id"), item.get("aggregated_output", "")
        elif item.get("type") == "mcp_tool_call":
            yield item.get("id"), output_text(item.get("result", {}))
    elif kind == "user":
        for block in event.get("message", {}).get("content", []):
            if block.get("type") == "tool_result":
                yield block.get("tool_use_id"), output_text(block.get("content", ""))
    elif kind == "tool_result":
        yield (event.get("agent_id", ""), event.get("id")), output_text(event.get("text", ""))


class RetryGuard:
    def __init__(self, path, limit=3):
        if isinstance(limit, bool) or not isinstance(limit, int) or limit < 1:
            raise ValueError("syntax_error_limit must be a positive integer")
        self.path, self.limit = path, limit
        self.offset, self.pending = 0, b""
        self.seen, self.counts = set(), {}

    def __call__(self):
        if not self.path.exists():
            return None
        with self.path.open("rb") as stream:
            stream.seek(self.offset)
            data = stream.read()
            self.offset = stream.tell()
        lines = (self.pending + data).split(b"\n")
        self.pending = lines.pop()  # A tool can be midway through writing JSON.
        for line in lines:
            try:
                event = json.loads(line)
            except ValueError:
                continue
            if not isinstance(event, dict):
                continue
            for ident, output in returned_outputs(event):
                if ident is None:
                    continue
                for error in set(SYNTAX_ERROR.findall(unwrap_error_envelopes(ANSI.sub("", output)))):
                    key = (ident, error)
                    if key in self.seen:
                        continue
                    self.seen.add(key)
                    self.counts[error] = self.counts.get(error, 0) + 1
                    if self.counts[error] >= self.limit:
                        return {"kind": "repeated_cli_syntax_error", "error": error,
                                "completed_tools": self.counts[error]}
        return None
