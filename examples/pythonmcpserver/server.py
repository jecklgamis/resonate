# /// script
# requires-python = ">=3.10"
# dependencies = ["mcp>=2,<3"]
# ///
"""An MCP server built on the official Python SDK, for checking that resonate
works against a server it did not write.

It serves the same tools as examples/testmcpserver (add, multiply, echo,
solve_quadratic, sleep and fail, plus a resource and a prompt), so the shipped
examples/mcp-*.yaml scenarios run against it unchanged. What differs is
everything the SDK decides for itself: how it frames replies, how it issues
(or does not issue) sessions, what it returns for bad arguments.

Run it (uv installs the SDK into a throwaway environment):

    uv run examples/pythonmcpserver/server.py                       # :8767, SSE replies, stateful
    uv run examples/pythonmcpserver/server.py --json-response       # plain JSON replies
    uv run examples/pythonmcpserver/server.py --stateless           # no sessions at all
    uv run examples/pythonmcpserver/server.py --session-idle-timeout 5

The four combinations of --json-response and --stateless are the variants the
interop test runs; see interop_test.go.
"""

import argparse
import asyncio
import math
from typing import Any

from mcp.server.mcpserver import MCPServer

mcp = MCPServer("resonate-python-example")


@mcp.tool()
def add(a: float, b: float) -> float:
    """Add two numbers."""
    return a + b


@mcp.tool()
def multiply(a: float, b: float) -> float:
    """Multiply two numbers."""
    return a * b


@mcp.tool()
def echo(message: str) -> str:
    """Return the message."""
    return message


@mcp.tool()
def solve_quadratic(a: float, b: float, c: float) -> dict[str, Any]:
    """Solve ax^2 + bx + c = 0."""
    if a == 0:
        raise ValueError("a must not be zero")
    d = b * b - 4 * a * c
    roots: list[float] = []
    if d > 0:
        roots = [(-b + math.sqrt(d)) / (2 * a), (-b - math.sqrt(d)) / (2 * a)]
    elif d == 0:
        roots = [-b / (2 * a)]
    return {"discriminant": d, "real_roots": len(roots), "roots": roots}


@mcp.tool()
async def sleep(ms: float) -> str:
    """Answer after ms milliseconds (at most 5000)."""
    if ms < 0:
        raise ValueError("ms must not be negative")
    await asyncio.sleep(min(ms, 5000) / 1000)
    return "slept"


@mcp.tool()
def fail() -> str:
    """Always fails."""
    raise RuntimeError("this tool always fails")


@mcp.resource("file:///demo.txt")
def demo() -> str:
    """A small text resource."""
    return "hello from the example MCP server"


@mcp.prompt()
def greet() -> str:
    """Say hello."""
    return "Say hello"


def main() -> None:
    p = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    p.add_argument("--host", default="127.0.0.1")
    p.add_argument("--port", type=int, default=8767)
    p.add_argument("--json-response", action="store_true", help="reply with plain JSON instead of an event stream")
    p.add_argument("--stateless", action="store_true", help="issue no sessions: every request stands alone")
    p.add_argument("--session-idle-timeout", type=float, default=1800, help="seconds a session may sit idle before the server forgets it")
    a = p.parse_args()
    mcp.run(
        "streamable-http",
        host=a.host,
        port=a.port,
        streamable_http_path="/mcp",
        json_response=a.json_response,
        stateless_http=a.stateless,
        session_idle_timeout=a.session_idle_timeout,
    )


if __name__ == "__main__":
    main()
