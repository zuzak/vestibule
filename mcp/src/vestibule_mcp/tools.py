"""Dynamic MCP tool registration from a parsed Vestibule manifest.

FastMCP infers a tool's JSON input schema from the Python function's
signature. For manifest-driven registration we therefore synthesise a
function object with the right ``__signature__`` and ``__annotations__``
for each endpoint, then hand it to ``FastMCP.add_tool``.

Each synthesised function is a thin shim: it forwards its keyword
arguments to Vestibule and returns the parsed JSON response.
"""

from __future__ import annotations

import inspect
import logging
import time
from collections.abc import Awaitable, Callable
from typing import Any

import httpx
from mcp.server.fastmcp import FastMCP

from . import metrics
from .manifest import Endpoint, Param

log = logging.getLogger(__name__)

# JSON schema type per manifest param type. Dates ride as strings so the
# MCP caller can pass aliases (today/yesterday/today-N); Vestibule resolves
# them server-side.
_PY_TYPE = {
    "string": str,
    "integer": int,
    "date": str,
}

# Wording tacked onto the param description when the param is a date.
_DATE_HINT = " (YYYY-MM-DD, or today/yesterday/tomorrow/today±N)"


def _make_tool_fn(
    endpoint: Endpoint,
    client: httpx.AsyncClient,
    vestibule_url: str,
) -> Callable[..., Awaitable[Any]]:
    """Build an async function whose signature matches ``endpoint.params``.

    The returned callable is what FastMCP introspects to derive the tool's
    input schema, and what it invokes on each tool call.
    """
    base = vestibule_url.rstrip("/")
    path = f"{base}/{endpoint.upstream}/{endpoint.name}"

    async def tool_fn(**kwargs: Any) -> Any:
        # Drop unset optional params so Vestibule doesn't see them as
        # literal-None values in the query string.
        params = {k: v for k, v in kwargs.items() if v is not None}
        start = time.monotonic()
        try:
            resp = await client.get(path, params=params, timeout=30.0)
        except httpx.HTTPError as e:
            metrics.upstream_errors.labels(endpoint.tool_name, "transport").inc()
            metrics.tool_calls.labels(endpoint.tool_name, "error").inc()
            raise RuntimeError(f"vestibule unreachable: {e}") from e
        finally:
            metrics.upstream_duration.labels(endpoint.tool_name).observe(
                time.monotonic() - start
            )

        if resp.is_success:
            metrics.tool_calls.labels(endpoint.tool_name, "ok").inc()
            try:
                return resp.json()
            except ValueError:
                return resp.text

        # 4xx/5xx: surface as an MCP tool error with Vestibule's status
        # and short message. Don't wrap or translate further.
        metrics.tool_calls.labels(endpoint.tool_name, str(resp.status_code)).inc()
        metrics.upstream_errors.labels(endpoint.tool_name, f"http_{resp.status_code}").inc()
        message = _short_error_message(resp)
        raise RuntimeError(f"vestibule returned {resp.status_code}: {message}")

    _attach_signature(tool_fn, endpoint.params)
    tool_fn.__name__ = endpoint.tool_name
    tool_fn.__doc__ = endpoint.description or None
    return tool_fn


def _attach_signature(fn: Callable, params: tuple[Param, ...]) -> None:
    sig_params: list[inspect.Parameter] = []
    annotations: dict[str, Any] = {}
    for p in params:
        py_type = _PY_TYPE.get(p.type, str)
        if p.required:
            # Required: no default, bare type. FastMCP places it in
            # "required" in the JSON schema.
            default = inspect.Parameter.empty
            annotation = py_type
        elif p.default:
            # Optional with a manifest-declared default.
            default = _coerce_default(p.default, py_type)
            annotation = py_type
        else:
            # Optional, no default. Default to None and mark the
            # annotation as Optional so FastMCP doesn't flag it required.
            # The tool body filters None out before calling Vestibule.
            default = None
            annotation = py_type | None
        sig_params.append(
            inspect.Parameter(
                p.name,
                inspect.Parameter.KEYWORD_ONLY,
                default=default,
                annotation=annotation,
            )
        )
        annotations[p.name] = annotation
    fn.__signature__ = inspect.Signature(  # type: ignore[attr-defined]
        parameters=sig_params, return_annotation=Any
    )
    fn.__annotations__ = {**annotations, "return": Any}


def _coerce_default(raw: str, py_type: type) -> Any:
    """Coerce a manifest-declared default (always a string) to the param's
    Python type. For dates, leave the alias form as-is — Vestibule will
    resolve it."""
    if py_type is int:
        try:
            return int(raw)
        except ValueError:
            log.warning("integer default %r does not parse; using as string", raw)
            return raw
    return raw


def _short_error_message(resp: httpx.Response) -> str:
    try:
        body = resp.json()
        if isinstance(body, dict) and isinstance(body.get("error"), str):
            return body["error"]
    except ValueError:
        pass
    # Fallback: first line of the body, capped, with whitespace normalised.
    text = " ".join((resp.text or "").split())
    return text[:200] if text else resp.reason_phrase or "no message"


def register_tools(
    mcp: FastMCP,
    endpoints: list[Endpoint],
    client: httpx.AsyncClient,
    vestibule_url: str,
) -> None:
    """Register one MCP tool per endpoint. Description and input schema
    come from the manifest; the tool name is the provenance-free name
    declared in the Vestibule config's ``mcp.tool_name``."""
    for ep in endpoints:
        if ep.endpoint_type != "rest":
            # GraphQL endpoints validate but return 501 on the Vestibule
            # side. Skip on the MCP side rather than exposing a tool that
            # always errors.
            log.warning(
                "skipping endpoint %s/%s: type %r is not supported by the MCP",
                ep.upstream,
                ep.name,
                ep.endpoint_type,
            )
            continue
        fn = _make_tool_fn(ep, client, vestibule_url)
        description = _description_with_param_hints(ep)
        mcp.add_tool(fn, name=ep.tool_name, description=description)


def _description_with_param_hints(ep: Endpoint) -> str:
    """Extend the endpoint description with date-format hints, which the
    MCP caller needs but which live on the Param rather than the Endpoint.
    Kept out of the schema so each tool retains a single coherent
    description field."""
    parts = [ep.description.strip()] if ep.description.strip() else []
    date_params = [p.name for p in ep.params if p.type == "date"]
    if date_params:
        parts.append(
            "Date parameters accept YYYY-MM-DD or the aliases today, yesterday, "
            "tomorrow, and today±N."
        )
    return "\n\n".join(parts) if parts else ""
