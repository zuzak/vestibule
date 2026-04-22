# vestibule

A generic HTTP proxy for authenticated upstream APIs.

Trusted clients call vestibule over a public ingress; vestibule holds the
upstream credentials and exposes a narrow, allowlisted view of the upstream
API. The credentials never leave the pod.

## What it does

- Fronts one or more upstream HTTP APIs behind `GET /<upstream>/<endpoint>?<params>`.
- Supports two upstream auth types: `form_login` (POST credentials, reuse
  session cookie) and `header` (static headers on every request).
- Typed query params per endpoint: `string`, `integer`, or `date`. Unknown
  inbound params are rejected with `400`. Missing required params likewise.
  Date params accept `YYYY-MM-DD` and the aliases `today`, `yesterday`,
  `tomorrow`, and `today±N` (N ≤ 3650), resolved in UTC.
- Optional jq filter per endpoint, applied to the upstream response before
  caching (`github.com/itchyny/gojq`).
- Enforces a per-endpoint `min_interval` floor on upstream hits: requests
  that arrive faster than the floor are served from a small in-memory cache.
- Retries once on the AWS ALB bare-403 stickiness race, and once on a
  `form_login` session expiry.
- Exposes `GET /_manifest` (un-auth-gated) with per-endpoint metadata
  suitable for generating tool schemas downstream. Excludes URLs, auth,
  filters, and min_interval — manifest reveals nothing a caller shouldn't
  know.
- Sends an honest `User-Agent`.
- Exposes Prometheus metrics on a separate port.

## What it does not do

- **No writes.** Only `GET` is supported. `POST`/`PUT`/`DELETE` return 405.
- **No response transformation.** Upstream JSON is passed through verbatim
  with the upstream status code.
- **No per-client rate limiting.** Politeness is toward the *upstream*, via
  `min_interval`. Per-client limits belong at the ingress.
- **No multi-user auth.** The inbound `apikey` check is a single shared key.

## Config

YAML, loaded from the path in `VESTIBULE_CONFIG`. `${VAR}` placeholders are
expanded from the process environment.

See [`example.yaml`](./example.yaml) for a full example.

### Fields

| Field | Type | Notes |
|---|---|---|
| `listen` | string | Address for the public HTTP server. Default `:8080`. |
| `metrics_listen` | string | Address for the Prometheus metrics server. Default `:9090`. |
| `api_key` | string | Optional. If set, inbound requests must carry `?apikey=<value>`. If empty or absent, inbound auth is skipped. |
| `upstreams.<name>.auth.type` | string | `form_login` or `header`. No other types are supported. |
| `upstreams.<name>.auth.login_url` | string | `form_login` only. Full URL of the login endpoint. |
| `upstreams.<name>.auth.login_method` | string | `form_login` only. HTTP method; defaults to `POST`. |
| `upstreams.<name>.auth.form_fields` | map | `form_login` only. Form fields posted as `application/x-www-form-urlencoded`. Typically includes credentials. |
| `upstreams.<name>.auth.headers` | map | `header` only. Headers added to every outbound request. |
| `upstreams.<name>.endpoints.<name>.type` | string | `rest` or `graphql`. Defaults to `rest`. `graphql` validates but returns 501 until implementation lands. |
| `upstreams.<name>.endpoints.<name>.url` | string | Full upstream URL (no query string — use `params`). |
| `upstreams.<name>.endpoints.<name>.description` | string | Human-readable description, surfaced via `/_manifest`. |
| `upstreams.<name>.endpoints.<name>.params.<name>` | map | Per-param schema: `type` (`string`/`integer`/`date`; defaults to `string`), `description`, `default`, `required`. `required` and `default` are mutually exclusive. |
| `upstreams.<name>.endpoints.<name>.filter` | string | Optional jq expression applied to upstream responses before caching. Compiled at config load; invalid expressions fail startup. |
| `upstreams.<name>.endpoints.<name>.min_interval` | duration | Minimum time between upstream hits for this endpoint + params combination. Go duration syntax, e.g. `30s`, `5m`. |

### Environment variable interpolation

`${FOO}` in the YAML is replaced with the value of `$FOO` from the process
environment. Unset variables resolve to an empty string. There is no default
syntax; keep secrets in the environment and let config validation fail
cleanly if one is missing.

## Routing

```
GET /<upstream>/<endpoint>[?param=value&...]  -> proxy to the upstream
GET /_manifest                                -> endpoint metadata (un-auth-gated)
GET /healthz                                  -> 200 "ok" (always)
```

The metrics server (default `:9090`) is a separate listener with no auth; it
is intended for pod-network scraping only and should never be exposed on the
public ingress.

## Security posture

**This is deliberately weak inbound auth.** The query-param `apikey` is
visible in every log along its path (Anthropic's fetcher, ingress access
logs, URL history). It is not the primary security layer.

The primary security layer is expected to be ingress-level restriction. A
typical deployment:

1. Ingress-nginx in front, with an IP allowlist restricting inbound to the
   specific client ranges (e.g. Anthropic's `web_fetch` egress ranges, plus
   an operator VPN or Tailscale network).
2. The `apikey` check exists to distinguish "a client on an allowlisted IP
   with the correct key" from "a client on an allowlisted IP without it" —
   specifically to reject other tenants sharing the allowlisted egress.

**Skipping the allowlist is not an option.** Without it, the `apikey` is the
only gate — and query-param keys are not designed to be secrets.

### Credential exposure

- Credentials (`form_fields`, `headers`) live only in memory and never land
  in logs, metric labels, or response bodies.
- Upstream errors returned to the client carry a short generic message and
  the upstream status code. No upstream response headers carrying cookies
  are forwarded; the response body is passed through as-is.

## Images

Multi-arch images (linux/arm64 + linux/amd64) are published to GitHub
Container Registry on every push to `main` and on every `v*` tag:

```
ghcr.io/zuzak/vestibule:main             # moving tag for latest main
ghcr.io/zuzak/vestibule:sha-<short>      # immutable per-commit tag
ghcr.io/zuzak/vestibule:<semver>         # on v-tags only
ghcr.io/zuzak/vestibule:latest           # on v-tags only
```

A GHCR package inherits the source repository's visibility when first
published. If the repo is private, the package is private; pulling from
Kubernetes then requires an image pull secret. Make the package public
(GitHub → vestibule → Packages → `vestibule` → Package settings →
Change visibility) once the code is cleared for public access.

## Deploying

Out of scope for this repo. A typical deployment shape:

1. Pull the image from `ghcr.io/zuzak/vestibule:<tag>`.
2. Mount the YAML config from a Kubernetes `Secret`, so credentials stay
   in the cluster's secret store and never enter this repo.
3. Point an ingress-nginx `Ingress` at the service with:
   - an IP allowlist annotation restricting source ranges,
   - optionally an additional basic-auth layer.
4. Scrape `/metrics` on port 9090 from Prometheus, in-cluster only.

Specifics live in `zuzak/kube`, not here.

## Adding a new upstream

1. Decide which auth type fits: `form_login` for cookie-based web logins,
   `header` for bearer tokens / API keys.
2. Add an entry under `upstreams:` in your config. List every endpoint you
   intend to expose, with an explicit `params` map (typed per param) and a
   conservative `min_interval`.
3. Store credentials in environment variables (or the equivalent Kubernetes
   Secret keys), and reference them via `${VAR}` in the config.
4. Restart the proxy.

New auth types (OAuth2, mTLS, …) are explicitly out of scope for the
supported auth list — see [`CLAUDE.md`](./CLAUDE.md) — and need sign-off to
add.

## Metrics

All metrics are under the `vestibule_` prefix and include `upstream` and
`endpoint` labels.

| Metric | Type | Notes |
|---|---|---|
| `vestibule_requests_total` | counter | Inbound requests, labelled by `status`. |
| `vestibule_upstream_requests_total` | counter | Outbound upstream requests, labelled by `status` (a transport error records status `error`). |
| `vestibule_min_interval_served_total` | counter | Inbound requests served from the `min_interval` floor cache. |
| `vestibule_upstream_request_duration_seconds` | histogram | Outbound request duration. |

Plus standard Go runtime and process collectors.

## Development

```
# Run tests
go test -race ./...

# Format / vet
gofmt -w .
go vet ./...

# Run locally against the example config
VESTIBULE_CONFIG=./example.yaml \
  EXAMPLE_FORM_EMAIL=a@b.com \
  EXAMPLE_FORM_PASSWORD=xxx \
  EXAMPLE_BEARER_TOKEN=xyz \
  PROXY_API_KEY=dev \
  go run .
```

Docker images are built in CI for `linux/arm64` and `linux/amd64`; no local
Docker build is required for development.
