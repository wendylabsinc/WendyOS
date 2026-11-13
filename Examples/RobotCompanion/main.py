"""A small robot app for exercising Wendy's ChatGPT gateway without motion."""

import threading
import time

from mcp.server.fastmcp import FastMCP
from mcp.types import ToolAnnotations

mcp = FastMCP("wendy-robot-companion", host="127.0.0.1", port=8091,
              stateless_http=True, streamable_http_path="/")
started = time.monotonic()
lock = threading.Lock()
message = "Ready to talk to ChatGPT."


@mcp.tool(annotations=ToolAnnotations(
    readOnlyHint=True, destructiveHint=False, idempotentHint=True, openWorldHint=False,
))
def get_status() -> dict:
    """Read the companion's message and uptime to verify application readiness."""
    with lock:
        return {"ready": True, "message": message, "uptime_seconds": round(time.monotonic() - started, 1)}


@mcp.tool(annotations=ToolAnnotations(
    readOnlyHint=False, destructiveHint=False, idempotentHint=True, openWorldHint=False,
))
def set_message(text: str) -> dict:
    """Set the companion's displayed message. Does not move the robot or play audio."""
    if not text.strip() or len(text) > 200:
        raise ValueError("Supply between 1 and 200 characters.")
    global message
    with lock:
        message = text
        return {"message": message}


if __name__ == "__main__":
    mcp.run(transport="streamable-http")
