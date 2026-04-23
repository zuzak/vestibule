"""Manifest parsing tests."""

from __future__ import annotations

import httpx
import pytest
import respx

from vestibule_mcp.manifest import (
    Endpoint,
    Param,
    fetch_manifest,
    fetch_manifest_sync,
    parse_manifest,
)

SAMPLE = {
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
                            "description": "YYYY-MM-DD, or today/yesterday/today-N",
                            "default": "today",
                            "required": False,
                        },
                    },
                    "mcp": {"tool_name": "get_food_diary"},
                },
                "weight": {
                    "type": "rest",
                    "description": "Weight progress.",
                    "params": {
                        "measureID": {
                            "type": "integer",
                            "required": True,
                        },
                    },
                    "mcp": {"tool_name": "get_body_weight"},
                },
                "internal": {
                    "type": "rest",
                    # No mcp block — must be skipped.
                    "params": {"x": {"type": "string"}},
                },
            },
        },
    },
}


def test_parse_extracts_endpoints_with_mcp_blocks():
    endpoints = parse_manifest(SAMPLE)
    names = {ep.tool_name for ep in endpoints}
    assert names == {"get_food_diary", "get_body_weight"}


def test_parse_skips_endpoints_without_mcp():
    endpoints = parse_manifest(SAMPLE)
    assert not any(ep.name == "internal" for ep in endpoints)


def test_parse_preserves_param_metadata():
    diary = next(
        ep for ep in parse_manifest(SAMPLE) if ep.tool_name == "get_food_diary"
    )
    assert diary.description == "Food diary for a given day."
    assert diary.upstream == "nutracheck"
    assert len(diary.params) == 1

    date_param = diary.params[0]
    assert date_param == Param(
        name="date",
        type="date",
        description="YYYY-MM-DD, or today/yesterday/today-N",
        default="today",
        required=False,
    )


def test_parse_handles_required_param():
    weight = next(
        ep for ep in parse_manifest(SAMPLE) if ep.tool_name == "get_body_weight"
    )
    assert weight.params[0].required is True


def test_parse_empty_manifest():
    assert parse_manifest({"upstreams": {}}) == []
    assert parse_manifest({}) == []


def test_parse_skips_empty_tool_name():
    payload = {
        "upstreams": {
            "x": {
                "endpoints": {
                    "a": {"type": "rest", "mcp": {"tool_name": ""}},
                    "b": {"type": "rest", "mcp": {}},
                }
            }
        }
    }
    assert parse_manifest(payload) == []


@pytest.mark.asyncio
async def test_fetch_retries_then_succeeds():
    url = "http://vestibule.example/_manifest"
    async with respx.mock:
        # Two failures then a success.
        respx.get(url).mock(
            side_effect=[
                httpx.ConnectError("boom"),
                httpx.Response(503, text="not ready"),
                httpx.Response(200, json=SAMPLE),
            ]
        )
        async with httpx.AsyncClient() as client:
            payload = await fetch_manifest(
                client,
                "http://vestibule.example",
                attempts=5,
                backoff_initial=0.01,
                backoff_max=0.01,
            )
    assert payload == SAMPLE


@pytest.mark.asyncio
async def test_fetch_raises_after_max_attempts():
    url = "http://vestibule.example/_manifest"
    async with respx.mock:
        respx.get(url).mock(return_value=httpx.Response(500))
        async with httpx.AsyncClient() as client:
            with pytest.raises(RuntimeError, match="failed to fetch manifest"):
                await fetch_manifest(
                    client,
                    "http://vestibule.example",
                    attempts=3,
                    backoff_initial=0.01,
                    backoff_max=0.01,
                )


def test_fetch_sync_retries_then_succeeds():
    url = "http://vestibule.example/_manifest"
    with respx.mock:
        respx.get(url).mock(
            side_effect=[
                httpx.ConnectError("boom"),
                httpx.Response(503, text="not ready"),
                httpx.Response(200, json=SAMPLE),
            ]
        )
        payload = fetch_manifest_sync(
            "http://vestibule.example",
            attempts=5,
            backoff_initial=0.01,
            backoff_max=0.01,
        )
    assert payload == SAMPLE


def test_fetch_sync_raises_after_max_attempts():
    url = "http://vestibule.example/_manifest"
    with respx.mock:
        respx.get(url).mock(return_value=httpx.Response(500))
        with pytest.raises(RuntimeError, match="failed to fetch manifest"):
            fetch_manifest_sync(
                "http://vestibule.example",
                attempts=3,
                backoff_initial=0.01,
                backoff_max=0.01,
            )
