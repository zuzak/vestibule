"""Prometheus metric primitives for the MCP server.

The metrics server listens on a separate port from the MCP transport, for
in-cluster scraping only. Never place the metrics port behind a public
ingress — see CLAUDE.md.
"""

from __future__ import annotations

from prometheus_client import Counter, Histogram, start_http_server

# Labelled by the MCP tool name, not the upstream-side endpoint name —
# upstream names are provenance that we deliberately hide.
tool_calls = Counter(
    "mcp_tool_calls_total",
    "Total MCP tool calls.",
    ["tool", "status"],
)

upstream_duration = Histogram(
    "mcp_upstream_duration_seconds",
    "Duration of outbound requests to Vestibule, per tool.",
    ["tool"],
)

upstream_errors = Counter(
    "mcp_upstream_errors_total",
    "Errors from Vestibule as observed by the MCP, per tool.",
    ["tool", "reason"],
)


def start_metrics_server(port: int) -> None:
    """Start the prometheus_client HTTP server on ``port``.

    The server runs on a daemon thread; no explicit lifecycle management
    is needed. Returns once the listener is up.
    """
    start_http_server(port)
