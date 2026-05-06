"""
WendyOS MCP Example

An MCP server that exposes device tools via the Model Context Protocol.
Deploy with `wendy run` — the mcp entitlement in wendy.json registers
this server with the wendy agent so its tools are automatically proxied
through `wendy mcp serve`.
"""

import os
import platform
import socket
import subprocess

import uvicorn
from mcp.server.fastmcp import FastMCP

MCP_PORT = int(os.environ.get("MCP_PORT", 3000))

mcp = FastMCP("wendy-example")


@mcp.tool()
def ping() -> str:
    """Check that this MCP server is reachable."""
    return "pong"


@mcp.tool()
def device_info() -> dict:
    """Return basic information about this WendyOS device."""
    return {
        "hostname": socket.gethostname(),
        "architecture": platform.machine(),
        "os": platform.system().lower(),
        "python": platform.python_version(),
    }


@mcp.tool()
def run_command(command: str) -> dict:
    """Run a shell command and return its output (stdout + stderr)."""
    result = subprocess.run(
        command,
        shell=True,
        capture_output=True,
        text=True,
        timeout=10,
    )
    return {
        "exit_code": result.returncode,
        "stdout": result.stdout,
        "stderr": result.stderr,
    }


if __name__ == "__main__":
    uvicorn.run(mcp.streamable_http_app(), host="0.0.0.0", port=MCP_PORT)
