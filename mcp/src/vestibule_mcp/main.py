"""Vestibule MCP entrypoint."""

from __future__ import annotations

import logging
import os
import sys
from collections.abc import AsyncIterator
from contextlib import asynccontextmanager

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


def _make_lifespan(vestibule_url: str):
    """Build the lifespan context manager FastMCP will call on startup.

    The httpx client, manifest fetch, and tool registration all happen
    inside the lifespan — which runs on the same event loop as the MCP
    server. Creating the client anywhere else would bind it to a
    different loop and every later tool call would fail with
    "Event loop is closed".
    """

    @asynccontextmanager
    async def lifespan(mcp: FastMCP) -> AsyncIterator[dict]:
        async with httpx.AsyncClient() as client:
            payload = await fetch_manifest(client, vestibule_url)
            endpoints = parse_manifest(payload)
            register_tools(mcp, endpoints, client, vestibule_url)
            tool_names = [ep.tool_name for ep in endpoints]
            log.info(
                "registered %d tools: %s",
                len(tool_names),
                ", ".join(tool_names) or "(none)",
            )
            yield {}

    return lifespan


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

    # json_response=True keeps responses as plain JSON rather than SSE
    # framing; simpler through ingress-nginx.
    #
    # We do NOT set stateless_http=True. In stateless mode FastMCP
    # invokes the lifespan per-request — which would close the httpx
    # client between requests and break every tool call. Stateful mode
    # runs the lifespan once at process start; the kube Deployment
    # pins replicas=1 so cross-request session state is safe.
    mcp = FastMCP(
        "vestibule-mcp",
        host="0.0.0.0",
        port=http_port,
        streamable_http_path=DEFAULT_MCP_PATH,
        json_response=True,
        lifespan=_make_lifespan(vestibule_url),
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
