# CLAUDE.md — vestibule

Permanent invariants for future work on this repo. Anything listed here
requires human sign-off to change, not just a PR that looks sensible.

## Public-repo hygiene

- No real upstream names, credentials, or endpoint hostnames in the repo —
  in code, tests, example config, docs, commit messages, or CI.
- The example config uses `example.com` only. Tests use `httptest.Server`,
  which binds to `127.0.0.1` on an ephemeral port.
- Real upstream configs live in a Kubernetes Secret applied out-of-band.
  That is the deployment contract; this repo never sees them.

This invariant applies whether the repo is public or private at any given
time. "It's private right now" is not a licence to relax it.

## Supported auth types

Only `form_login` and `header` are supported. Adding any other type
(OAuth2, mTLS, signed requests, assume-role, …) is explicitly out of scope
and needs human sign-off — not just a PR. Raising the question first, in an
issue or Discussion on `zuzak/.claude`, is the right move.

## Style

- Flat Go. Everything in `package main` at the repo root. No `internal/`
  subpackages; no cross-package abstractions invented for their own sake.
- Standard library first. Acceptable third-party deps: `gopkg.in/yaml.v3`,
  `github.com/prometheus/client_golang`. Adding a new dependency needs a
  specific reason tied to the change; do not reach for a framework.
- Interfaces only where a test genuinely needs one. A struct that is only
  used in one place does not need an interface "for future flexibility".
- `main.go` should be the reader's entry point. Do not bury core logic
  behind layers of indirection.

## Politeness invariants

These are contracts with upstreams, not style preferences. Do not regress
any of them.

- Honest `User-Agent`: `vestibule/<version> (+https://github.com/zuzak/vestibule)`.
  No browser spoofing. No anonymising UA.
- Per-endpoint `min_interval` floor. Inbound requests that arrive faster
  than the floor must be served from the in-memory cache. The floor is
  applied per `(upstream, endpoint, canonical-params)` — the same inbound
  URL served repeatedly inside the floor must only hit upstream once.
- At most one re-login attempt per inbound request on a `form_login`
  upstream. Do not add a loop. If the re-login fails, return 502 to the
  client and stop.
- At most one ALB-bare-403 retry per inbound request, with a short delay
  (currently ~1s) before the retry. Do not add exponential backoff or
  further retries.
- Concurrent re-login attempts against the same upstream must be
  serialised by a per-upstream mutex. N concurrent inbound requests seeing
  a 401 must not fan out into N parallel login requests.
- Every outbound HTTP call must have a timeout. `http.DefaultClient` must
  not be used for upstream calls.

## Permanently out of scope

- **Writes to upstreams.** Only `GET` is forwarded. No `POST`/`PUT`/
  `PATCH`/`DELETE` proxying, even if an upstream's API supports it.
- **Response transformation or normalisation.** Upstream JSON passes
  through verbatim. If a caller wants a different shape, they transform
  on their end.
- **Inbound auth.** None. Vestibule trusts its network boundary — see
  "Inbound exposure" below. Do not add apikeys, JWT verification, OIDC,
  or anything else to the application layer.
- **Per-client rate limiting.** Politeness is toward the *upstream*; that
  is what `min_interval` is for. Per-client rate limiting belongs at the
  ingress in front of any future consumer (e.g. the MCP), not here.
- **Credential exposure.** Credentials must never appear in logs, metric
  labels, request paths, response bodies, or error messages returned to
  inbound clients. Review any new logging call against this.

## Inbound exposure

Vestibule is an internal service. It must not be exposed via public
ingress. The MCP server in `mcp/` is the intended consumer; direct
external use is not supported. There is no application-layer inbound
auth — the network boundary (ClusterIP, no ingress) is the whole
security story.

Do not add inbound auth back, do not add per-client rate limiting, do not
add anything that implies external consumers. If a future requirement
pushes in that direction, the answer is a new layer in front (the MCP,
or another component) — not making Vestibule public.

## Testing posture

- The acceptance criteria in the initial build included specific tests:
  config loading with env interpolation, typed param validation,
  `min_interval` floor, `form_login` cookie reuse, ALB 403 retry. These
  tests must continue to pass; if a refactor changes behaviour that one
  of them covers, update the test deliberately — do not weaken the
  assertion.
- Tests must not depend on real network endpoints. Use `httptest.Server`
  for every upstream interaction under test.

## Schema evolution invariants

- **Date aliases resolve in UTC.** Accepted forms are `YYYY-MM-DD`,
  `today`, `yesterday`, `tomorrow`, and `today±N` (non-negative integer
  N ≤ 3650). A configurable timezone may be added later, but until then
  no alternate resolution behaviour is acceptable. Cache keys use the
  resolved canonical date, so `date=today` and the equivalent literal
  share an entry.
- **Filters are config-time, not request-time.** The jq expression is
  part of the endpoint definition, compiled at config load, and applied
  to the upstream response before caching. Per-request filters would
  require keying the cache on the filter expression and are out of
  scope.
- **The `/_manifest` endpoint is un-auth-gated.** Every field on
  `Endpoint` must be explicitly evaluated for manifest inclusion;
  default is exclude. The manifest is populated from a dedicated DTO
  (`manifestEndpoint` and friends in `proxy.go`), not by marshalling
  `Endpoint` directly — this way a new `Endpoint` field is invisible
  in the manifest until someone deliberately opts it in. Never expose
  `url`, `auth`, `filter`, `min_interval`, or anything else that could
  reveal a credential or an upstream URL.
- **GraphQL endpoint type is reserved.** `type: graphql` validates at
  config load but requests to such an endpoint return 501 Not
  Implemented. Implementation is a separate, tracked piece of work.
