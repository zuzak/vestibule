"""Tool registration tests."""

from __future__ import annotations

import httpx
import pytest
from mcp.server.fastmcp import FastMCP

from vestibule_mcp.manifest import Endpoint, Param
from vestibule_mcp.tools import register_tools


def _endpoints() -> list[Endpoint]:
    return [
        Endpoint(
            upstream="nutracheck",
            name="diary",
            tool_name="get_food_diary",
            endpoint_type="rest",
            description="Food diary for a day.",
            params=(
                Param(
                    name="date",
                    type="date",
                    description="YYYY-MM-DD, or today/yesterday/today-N",
                    default="today",
                ),
            ),
        ),
        Endpoint(
            upstream="nutracheck",
            name="weight",
            tool_name="get_body_weight",
            endpoint_type="rest",
            description="Weight data.",
            params=(
                Param(name="measureID", type="integer", required=True),
                Param(name="order", type="string"),
            ),
        ),
        Endpoint(
            upstream="x",
            name="ql",
            tool_name="ql_tool",
            endpoint_type="graphql",  # must be skipped
        ),
    ]


@pytest.mark.asyncio
async def test_register_adds_rest_tools_only():
    mcp = FastMCP("test")
    async with httpx.AsyncClient() as client:
        register_tools(mcp, _endpoints(), client, "http://v.example")

        tools = await mcp.list_tools()
        names = {t.name for t in tools}
        assert names == {"get_food_diary", "get_body_weight"}
        # GraphQL endpoint skipped.
        assert "ql_tool" not in names


@pytest.mark.asyncio
async def test_registered_tool_has_manifest_description():
    mcp = FastMCP("test")
    async with httpx.AsyncClient() as client:
        register_tools(mcp, _endpoints(), client, "http://v.example")

    tools = {t.name: t for t in await mcp.list_tools()}
    desc = tools["get_food_diary"].description or ""
    assert "Food diary for a day." in desc
    # Because there's a date param, the description gets a hint.
    assert "YYYY-MM-DD" in desc


@pytest.mark.asyncio
async def test_registered_tool_input_schema_reflects_params():
    mcp = FastMCP("test")
    async with httpx.AsyncClient() as client:
        register_tools(mcp, _endpoints(), client, "http://v.example")

    tools = {t.name: t for t in await mcp.list_tools()}
    diary_schema = tools["get_food_diary"].inputSchema
    assert diary_schema["type"] == "object"
    assert "date" in diary_schema["properties"]
    assert diary_schema["properties"]["date"]["type"] == "string"
    # Optional param with a default; not in required list.
    assert "date" not in diary_schema.get("required", [])
    assert diary_schema["properties"]["date"].get("default") == "today"

    weight_schema = tools["get_body_weight"].inputSchema
    assert weight_schema["properties"]["measureID"]["type"] == "integer"
    # measureID is required.
    assert "measureID" in weight_schema.get("required", [])
    # order is optional.
    assert "order" not in weight_schema.get("required", [])


@pytest.mark.asyncio
async def test_no_description_hint_without_date_param():
    ep = [
        Endpoint(
            upstream="x",
            name="e",
            tool_name="no_date_tool",
            endpoint_type="rest",
            description="Just a string param.",
            params=(Param(name="status", type="string"),),
        )
    ]
    mcp = FastMCP("test")
    async with httpx.AsyncClient() as client:
        register_tools(mcp, ep, client, "http://v.example")
    tools = {t.name: t for t in await mcp.list_tools()}
    desc = tools["no_date_tool"].description or ""
    assert "YYYY-MM-DD" not in desc
