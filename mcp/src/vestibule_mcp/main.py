"""Vestibule MCP entrypoint."""

from __future__ import annotations

import logging
import os
import sys

import httpx
from mcp.server.fastmcp import FastMCP
from starlette.responses import PlainTextResponse

from . import metrics
from .manifest import fetch_manifest_sync, parse_manifest
from .tools import register_tools

log = logging.getLogger("vestibule_mcp")

DEFAULT_HTTP_PORT = 8080
DEFAULT_METRICS_PORT = 9090
DEFAULT_MCP_PATH = "/mcp"


def _configure_logging() -> None:
    logging.basicConfig(
        level=os.environ.get("LOG_LEVEL", "INFO").upper(),
        format="%(asctime)s %(levelname)s %(name)s %(message)s",
    )


def _port(env: str, default: int) -> int:
    raw = os.environ.get(env)
    if not raw:
        return default
    try:
        return int(raw)
    except ValueError:
        log.error("invalid %s=%r; using default %d", env, raw, default)
        return default


def _add_healthz(mcp: FastMCP) -> None:
    """Add /healthz at the root of FastMCP's Starlette app (alongside /mcp,
    not nested under it). Confirmed empirically on the installed SDK."""

    @mcp.custom_route("/healthz", methods=["GET"])
    async def healthz(request):  # noqa: ANN001 — Starlette request type
        return PlainTextResponse("ok\n")


def main() -> None:
    _configure_logging()

    vestibule_url = os.environ.get("VESTIBULE_URL")
    if not vestibule_url:
        raise SystemExit("VESTIBULE_URL not set")

    http_port = _port("MCP_HTTP_PORT", DEFAULT_HTTP_PORT)
    metrics_port = _port("MCP_METRICS_PORT", DEFAULT_METRICS_PORT)

    # Fetch the manifest and register tools BEFORE starting the server.
    # FastMCP's user-provided lifespan only runs inside lowlevel
    # Server.run(), which is invoked per MCP session — not at process
    # start — so registering inside the lifespan leaves the server with
    # zero tools until a client opens a session. Doing it here means
    # the very first tools/list poll sees the full set.
    payload = fetch_manifest_sync(vestibule_url)
    endpoints = parse_manifest(payload)

    # httpx.AsyncClient is constructed sync; it binds to whichever event
    # loop runs the first await on it. uvicorn provides a single loop
    # for all requests, so all tool calls share this client.
    client = httpx.AsyncClient()

    # json_response=True keeps responses as plain JSON rather than SSE
    # framing; simpler through ingress-nginx.
    mcp = FastMCP(
        "vestibule-mcp",
        host="0.0.0.0",
        port=http_port,
        streamable_http_path=DEFAULT_MCP_PATH,
        json_response=True,
    )

    register_tools(mcp, endpoints, client, vestibule_url)
    tool_names = [ep.tool_name for ep in endpoints]
    log.info(
        "registered %d tools: %s",
        len(tool_names),
        ", ".join(tool_names) or "(none)",
    )

    _add_healthz(mcp)

    metrics.start_metrics_server(metrics_port)
    log.info("metrics listening on :%d", metrics_port)

    log.info("serving MCP on :%d%s", http_port, DEFAULT_MCP_PATH)
    mcp.run(transport="streamable-http")


if __name__ == "__main__":  # pragma: no cover
    try:
        main()
    except KeyboardInterrupt:
        sys.exit(0)
