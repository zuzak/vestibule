"""Vestibule MCP entrypoint."""

from __future__ import annotations

import asyncio
import logging
import os
import sys

import httpx
from mcp.server.fastmcp import FastMCP
from starlette.responses import PlainTextResponse

from . import metrics
from .manifest import fetch_manifest, parse_manifest
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


async def _startup(mcp: FastMCP) -> httpx.AsyncClient:
    """Fetch manifest and register tools. Returns the httpx client so the
    caller can close it on shutdown."""
    vestibule_url = os.environ.get("VESTIBULE_URL")
    if not vestibule_url:
        raise SystemExit("VESTIBULE_URL not set")

    client = httpx.AsyncClient()
    payload = await fetch_manifest(client, vestibule_url)
    endpoints = parse_manifest(payload)
    register_tools(mcp, endpoints, client, vestibule_url)

    tool_names = [ep.tool_name for ep in endpoints]
    log.info("registered %d tools: %s", len(tool_names), ", ".join(tool_names) or "(none)")
    return client


def _add_healthz(mcp: FastMCP) -> None:
    """Add /healthz as a custom route on FastMCP's Starlette app.

    FastMCP exposes @custom_route() which registers the handler on the
    same ASGI app as the MCP transport. Cheaper than wrapping in a
    parent Starlette, and keeps the readiness check on the same port
    the MCP is served on.
    """

    @mcp.custom_route("/healthz", methods=["GET"])
    async def healthz(request):  # noqa: ANN001 — Starlette request type
        return PlainTextResponse("ok\n")


def main() -> None:
    _configure_logging()

    http_port = _port("MCP_HTTP_PORT", DEFAULT_HTTP_PORT)
    metrics_port = _port("MCP_METRICS_PORT", DEFAULT_METRICS_PORT)

    # stateless_http=True avoids the in-memory session state that would
    # otherwise pin us to a single replica forever. json_response=True
    # keeps responses as plain JSON rather than SSE framing; simpler
    # through ingress-nginx.
    mcp = FastMCP(
        "vestibule-mcp",
        host="0.0.0.0",
        port=http_port,
        streamable_http_path=DEFAULT_MCP_PATH,
        stateless_http=True,
        json_response=True,
    )

    _add_healthz(mcp)

    # Metrics server first so it's up even if manifest fetch is slow.
    metrics.start_metrics_server(metrics_port)
    log.info("metrics listening on :%d", metrics_port)

    # Synchronously fetch manifest and register tools before starting
    # the MCP server. A half-initialised server that reports "ready" but
    # has no tools is worse than one that takes a few seconds longer.
    asyncio.run(_startup(mcp))

    log.info("serving MCP on :%d%s", http_port, DEFAULT_MCP_PATH)
    mcp.run(transport="streamable-http")


if __name__ == "__main__":  # pragma: no cover
    try:
        main()
    except KeyboardInterrupt:
        sys.exit(0)
