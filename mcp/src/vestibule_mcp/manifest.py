"""Fetch and parse Vestibule's /_manifest."""

from __future__ import annotations

import logging
from dataclasses import dataclass, field

import httpx

log = logging.getLogger(__name__)


@dataclass(frozen=True)
class Param:
    name: str
    type: str  # "string" | "integer" | "date"
    description: str = ""
    default: str = ""
    required: bool = False


@dataclass(frozen=True)
class Endpoint:
    upstream: str
    name: str  # endpoint name within the upstream
    tool_name: str  # the MCP tool name — provenance-free
    endpoint_type: str  # "rest" | "graphql"
    description: str = ""
    params: tuple[Param, ...] = field(default_factory=tuple)


def parse_manifest(payload: dict) -> list[Endpoint]:
    """Extract the list of MCP-exposed endpoints from a raw manifest dict.

    Endpoints without an ``mcp.tool_name`` are skipped — they are reachable
    via Vestibule but not intended for MCP exposure.
    """
    endpoints: list[Endpoint] = []
    for upstream_name, upstream in (payload.get("upstreams") or {}).items():
        for endpoint_name, endpoint in (upstream.get("endpoints") or {}).items():
            mcp_block = endpoint.get("mcp") or {}
            tool_name = mcp_block.get("tool_name") or ""
            if not tool_name:
                continue

            params = tuple(
                Param(
                    name=pname,
                    type=(pspec.get("type") or "string"),
                    description=pspec.get("description") or "",
                    default=pspec.get("default") or "",
                    required=bool(pspec.get("required")),
                )
                for pname, pspec in (endpoint.get("params") or {}).items()
            )

            endpoints.append(
                Endpoint(
                    upstream=upstream_name,
                    name=endpoint_name,
                    tool_name=tool_name,
                    endpoint_type=endpoint.get("type") or "rest",
                    description=endpoint.get("description") or "",
                    params=params,
                )
            )
    return endpoints


async def fetch_manifest(
    client: httpx.AsyncClient,
    vestibule_url: str,
    *,
    attempts: int = 10,
    backoff_initial: float = 0.5,
    backoff_max: float = 8.0,
) -> dict:
    """GET {vestibule_url}/_manifest with retries.

    This is a startup-ordering concern — Vestibule may not be ready yet —
    so we retry a handful of times with exponential backoff before giving
    up. After startup the MCP does not re-fetch.
    """
    url = vestibule_url.rstrip("/") + "/_manifest"
    delay = backoff_initial
    last_err: Exception | None = None
    for attempt in range(1, attempts + 1):
        try:
            resp = await client.get(url, timeout=10.0)
            resp.raise_for_status()
            return resp.json()
        except (httpx.HTTPError, ValueError) as e:
            last_err = e
            log.warning("manifest fetch attempt %d failed: %s", attempt, e)
            if attempt == attempts:
                break
            import asyncio

            await asyncio.sleep(delay)
            delay = min(delay * 2, backoff_max)
    raise RuntimeError(f"failed to fetch manifest after {attempts} attempts: {last_err}")


def fetch_manifest_sync(
    vestibule_url: str,
    *,
    attempts: int = 10,
    backoff_initial: float = 0.5,
    backoff_max: float = 8.0,
) -> dict:
    """Synchronous counterpart to fetch_manifest, for use at process
    startup before any event loop is running. Same retry semantics."""
    import time

    url = vestibule_url.rstrip("/") + "/_manifest"
    delay = backoff_initial
    last_err: Exception | None = None
    with httpx.Client() as client:
        for attempt in range(1, attempts + 1):
            try:
                resp = client.get(url, timeout=10.0)
                resp.raise_for_status()
                return resp.json()
            except (httpx.HTTPError, ValueError) as e:
                last_err = e
                log.warning("manifest fetch attempt %d failed: %s", attempt, e)
                if attempt == attempts:
                    break
                time.sleep(delay)
                delay = min(delay * 2, backoff_max)
    raise RuntimeError(f"failed to fetch manifest after {attempts} attempts: {last_err}")
