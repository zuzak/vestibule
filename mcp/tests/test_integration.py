"""End-to-end: fake Vestibule, register tools, invoke a tool, verify the
upstream is called correctly and the response flows back."""

from __future__ import annotations

import httpx
import pytest
import respx
from mcp.server.fastmcp import FastMCP

from vestibule_mcp.manifest import parse_manifest
from vestibule_mcp.tools import register_tools

MANIFEST = {
    "version": "test",
    "upstreams": {
        "nutracheck": {
            "endpoints": {
                "diary": {
                    "type": "rest",
                    "description": "Food diary for a given day.",
                    "params": {
                        "date": {
                            "type": "date",
                            "default": "today",
                        },
                    },
                    "mcp": {"tool_name": "get_food_diary"},
                },
            },
        },
    },
}


@pytest.mark.asyncio
async def test_tool_call_hits_vestibule_and_returns_json():
    vestibule_url = "http://vestibule.example"
    diary_url = f"{vestibule_url}/nutracheck/diary"

    async with respx.mock:
        route = respx.get(diary_url).mock(
            return_value=httpx.Response(
                200,
                json={"entries": [{"food": "oats", "kcal": 300}]},
            )
        )

        async with httpx.AsyncClient() as client:
            mcp = FastMCP("test")
            endpoints = parse_manifest(MANIFEST)
            register_tools(mcp, endpoints, client, vestibule_url)

            result = await mcp.call_tool("get_food_diary", {"date": "2026-04-22"})

    # respx records the request — verify the upstream call was right.
    assert route.call_count == 1
    called = route.calls.last.request
    assert called.url.params["date"] == "2026-04-22"

    # The tool result is delivered as MCP content; unwrap.
    structured = _structured(result)
    assert structured == {"entries": [{"food": "oats", "kcal": 300}]}


@pytest.mark.asyncio
async def test_tool_call_uses_default_when_param_omitted():
    vestibule_url = "http://vestibule.example"
    diary_url = f"{vestibule_url}/nutracheck/diary"

    async with respx.mock:
        route = respx.get(diary_url).mock(
            return_value=httpx.Response(200, json={"entries": []})
        )

        async with httpx.AsyncClient() as client:
            mcp = FastMCP("test")
            endpoints = parse_manifest(MANIFEST)
            register_tools(mcp, endpoints, client, vestibule_url)

            await mcp.call_tool("get_food_diary", {})

    called = route.calls.last.request
    # Default goes into the outbound query string — Vestibule resolves the
    # alias server-side.
    assert called.url.params["date"] == "today"


@pytest.mark.asyncio
async def test_tool_call_surfaces_upstream_error():
    vestibule_url = "http://vestibule.example"
    diary_url = f"{vestibule_url}/nutracheck/diary"

    async with respx.mock:
        respx.get(diary_url).mock(
            return_value=httpx.Response(
                502, json={"error": "upstream request failed"}
            )
        )

        async with httpx.AsyncClient() as client:
            mcp = FastMCP("test")
            endpoints = parse_manifest(MANIFEST)
            register_tools(mcp, endpoints, client, vestibule_url)

            # FastMCP raises ToolError when the tool function raises.
            # Verify the message contains both the status and Vestibule's
            # error string.
            from mcp.server.fastmcp.exceptions import ToolError

            with pytest.raises(ToolError) as excinfo:
                await mcp.call_tool("get_food_diary", {"date": "2026-04-22"})

    text = str(excinfo.value)
    assert "502" in text
    assert "upstream request failed" in text


def _content_text(result) -> str:
    """Concatenate text from any MCP tool-call result shape.

    FastMCP.call_tool has returned different shapes across SDK versions:
    an iterable of content blocks, or a (content, structured) tuple.
    Walk whatever we get and pull TextContent.text out.
    """
    texts: list[str] = []

    def walk(x):
        if x is None:
            return
        if isinstance(x, (list, tuple)):
            for item in x:
                walk(item)
            return
        text = getattr(x, "text", None)
        if isinstance(text, str):
            texts.append(text)

    walk(result)
    return " ".join(texts)


def _structured(result):
    """Extract a structured (JSON-ish) payload from a tool-call result,
    falling back to parsing the text block."""
    # Some SDK versions return (content, structured).
    if isinstance(result, tuple) and len(result) == 2:
        _, structured = result
        if structured is not None:
            # FastMCP often wraps a raw JSON value as {"result": value}
            # when the tool return type isn't a declared pydantic model.
            if isinstance(structured, dict) and set(structured.keys()) == {"result"}:
                return structured["result"]
            return structured
    # Otherwise try to parse the first text block as JSON.
    import json

    text = _content_text(result)
    if text:
        try:
            return json.loads(text)
        except ValueError:
            return text
    return None
