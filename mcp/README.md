# vestibule-mcp

MCP server that consumes [Vestibule](../README.md)'s `/_manifest` and
exposes the configured endpoints as MCP tools.

Vestibule holds the upstream credentials and politeness contracts; this
server translates between MCP and Vestibule and does nothing else. Think
of it as the presentation layer.

## What it does

- Fetches `{VESTIBULE_URL}/_manifest` at startup, retries a handful of
  times if Vestibule isn't ready yet.
- For each endpoint whose manifest entry has an `mcp.tool_name`,
  registers an MCP tool with that name.
- Tool descriptions are taken verbatim from the manifest. Input schemas
  are derived from the manifest's `params`.
- On tool call: GET `{VESTIBULE_URL}/{upstream}/{endpoint}?<params>`,
  return the parsed JSON. On non-2xx from Vestibule, surface the status
  and short error message to the MCP caller.

## Runtime

| Port | Purpose | Public? |
|---|---|---|
| 8080 | Streamable HTTP MCP, path `/mcp`, also `/healthz` | Yes (via ingress + basic auth) |
| 9090 | Prometheus scrape endpoint | **No — cluster only** |

## Config

| Env var | Required | Notes |
|---|---|---|
| `VESTIBULE_URL` | yes | Vestibule's in-cluster URL, e.g. `http://vestibule.claude-vestibule.svc.cluster.local:8080`. |
| `MCP_HTTP_PORT` | no | Defaults to 8080. |
| `MCP_METRICS_PORT` | no | Defaults to 9090. |
| `LOG_LEVEL` | no | Python logging level; defaults to `INFO`. |

## Metrics

All metrics under the `mcp_` prefix and labelled by the MCP-visible
tool name (never the upstream or endpoint name — those would leak
provenance):

| Metric | Type | Labels |
|---|---|---|
| `mcp_tool_calls_total` | counter | `tool`, `status` (`ok`, `error`, or status code string) |
| `mcp_upstream_duration_seconds` | histogram | `tool` |
| `mcp_upstream_errors_total` | counter | `tool`, `reason` (`transport` or `http_<code>`) |

Plus standard `prometheus_client` defaults.

## Development

```
# Create a venv and install with dev deps:
python3 -m venv .venv
./.venv/bin/pip install -e '.[dev]'

# Run tests:
./.venv/bin/pytest

# Run locally against a Vestibule instance:
VESTIBULE_URL=http://localhost:8080 ./.venv/bin/vestibule-mcp
```

## Invariants

See [`CLAUDE.md`](./CLAUDE.md) — things that must not change without
human sign-off: translation-layer discipline, no caching/retries, no
upstream provenance in tool names, streamable-HTTP-only transport.
