# CLAUDE.md — vestibule MCP

Invariants specific to this subdirectory. Anything listed here requires
human sign-off to change; a PR that *looks* sensible is not enough.

## Translation layer only

The MCP is a thin translation layer. No caching, no retries beyond what
httpx gives by default (one connection retry). Vestibule handles
politeness toward upstream; the MCP trusts Vestibule's responses. If a
Vestibule response is slow, that's Vestibule's business, not ours.

## Tools are generated from the manifest

Tools are generated dynamically from Vestibule's `/_manifest` at startup.
Adding a new tool is done by editing the Vestibule config and restarting
the MCP — not by writing Python. The registration code in `tools.py`
should never grow an endpoint-specific branch.

## No provenance in tool names

A tool's name is the MCP-visible identifier that appears in permission
prompts. It must not expose upstream provenance (no upstream names,
no vendor names). Tool names are declared in the Vestibule config's
`mcp.tool_name` field; the MCP is a consumer, not a namer — if a tool
name is wrong, fix the Vestibule config.

## Transport: streamable HTTP only

The MCP serves streamable HTTP on port 8080, path `/mcp`. Do not add
SSE transport — it's legacy, and ingress-nginx buffering makes it
unreliable in practice. Do not add stdio either (that's for
developer-laptop inspection via `mcp dev`, not for in-cluster use).

Use `stateless_http=True` and `json_response=True`. Session state would
pin us to a single replica forever and add a session-recovery mess; we
want neither.

## Metrics port is not public

`:9090` is the Prometheus scrape port. In-cluster traffic only. If an
ingress ever routes to `:9090`, that's a bug.

## Startup ordering

The MCP fetches the manifest once at startup. Vestibule must be Ready
before the MCP starts; the kube-side sync-wave ordering is the
enforcement. If Vestibule isn't reachable, the MCP retries a few times
with backoff and then fails to start. Do not paper over a missing
Vestibule by falling through to an empty tool set — that would
misrepresent capabilities to the caller.
