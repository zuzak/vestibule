package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/itchyny/gojq"
	"github.com/prometheus/client_golang/prometheus"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testMetrics() *metrics {
	return newMetrics(prometheus.NewRegistry())
}

func newTestProxy(t *testing.T, cfg *Config) *proxy {
	t.Helper()
	p, err := newProxy(cfg, testLogger(), testMetrics(), "test")
	if err != nil {
		t.Fatalf("newProxy: %v", err)
	}
	return p
}

// mustCompileFilter compiles a jq expression or fails the test. For inline
// test configs that bypass LoadConfig.
func mustCompileFilter(t *testing.T, expr string) *gojq.Code {
	t.Helper()
	parsed, err := gojq.Parse(expr)
	if err != nil {
		t.Fatalf("parse filter %q: %v", expr, err)
	}
	compiled, err := gojq.Compile(parsed)
	if err != nil {
		t.Fatalf("compile filter %q: %v", expr, err)
	}
	return compiled
}

// TestParamTypeValidation covers integer/date/string param types.
func TestParamTypeValidation(t *testing.T) {
	var received url.Values
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = r.URL.Query()
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cfg := &Config{
		Upstreams: map[string]Upstream{
			"demo": {
				Auth: AuthConfig{Type: "header", Headers: map[string]string{"X-T": "t"}},
				Endpoints: map[string]Endpoint{
					"things": {
						URL: upstream.URL,
						Params: map[string]Param{
							"limit": {Type: ParamTypeInteger},
							"date":  {Type: ParamTypeDate},
							"tag":   {Type: ParamTypeString},
						},
					},
				},
			},
		},
	}
	p := newTestProxy(t, cfg)
	p.now = fixedClock("2026-04-22")
	srv := httptest.NewServer(p.buildMux())
	defer srv.Close()

	// integer: valid
	r, _ := http.Get(srv.URL + "/demo/things?limit=42")
	r.Body.Close()
	if r.StatusCode != 200 || received.Get("limit") != "42" {
		t.Errorf("valid integer: status=%d received limit=%q", r.StatusCode, received.Get("limit"))
	}

	// integer: garbage → 400
	r2, _ := http.Get(srv.URL + "/demo/things?limit=abc")
	r2.Body.Close()
	if r2.StatusCode != 400 {
		t.Errorf("non-integer should 400, got %d", r2.StatusCode)
	}

	// date: garbage → 400
	r3, _ := http.Get(srv.URL + "/demo/things?date=garbage")
	r3.Body.Close()
	if r3.StatusCode != 400 {
		t.Errorf("invalid date should 400, got %d", r3.StatusCode)
	}

	// string: anything accepted
	r4, _ := http.Get(srv.URL + "/demo/things?tag=%22weird%40value%22")
	r4.Body.Close()
	if r4.StatusCode != 200 {
		t.Errorf("string param should pass: got %d", r4.StatusCode)
	}
}

// TestRequiredParam: missing required param returns 400 and names the param.
func TestRequiredParam(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`ok`))
	}))
	defer upstream.Close()

	cfg := &Config{
		Upstreams: map[string]Upstream{
			"demo": {
				Auth: AuthConfig{Type: "header", Headers: map[string]string{"X-T": "t"}},
				Endpoints: map[string]Endpoint{
					"things": {
						URL: upstream.URL,
						Params: map[string]Param{
							"id": {Type: ParamTypeString, Required: true},
						},
					},
				},
			},
		},
	}
	p := newTestProxy(t, cfg)
	srv := httptest.NewServer(p.buildMux())
	defer srv.Close()

	r, _ := http.Get(srv.URL + "/demo/things")
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if r.StatusCode != 400 {
		t.Errorf("expected 400, got %d", r.StatusCode)
	}
	if !strings.Contains(string(body), "id") {
		t.Errorf("error body should name the missing param %q; got %s", "id", body)
	}
}

// TestDefaultParam: missing optional param with a default uses the default.
func TestDefaultParam(t *testing.T) {
	var received string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = r.URL.Query().Get("status")
		_, _ = w.Write([]byte(`ok`))
	}))
	defer upstream.Close()

	cfg := &Config{
		Upstreams: map[string]Upstream{
			"demo": {
				Auth: AuthConfig{Type: "header", Headers: map[string]string{"X-T": "t"}},
				Endpoints: map[string]Endpoint{
					"things": {
						URL: upstream.URL,
						Params: map[string]Param{
							"status": {Type: ParamTypeString, Default: "active"},
						},
					},
				},
			},
		},
	}
	p := newTestProxy(t, cfg)
	srv := httptest.NewServer(p.buildMux())
	defer srv.Close()

	r, _ := http.Get(srv.URL + "/demo/things")
	r.Body.Close()
	if r.StatusCode != 200 || received != "active" {
		t.Errorf("default not applied: status=%d received=%q", r.StatusCode, received)
	}
}

// TestDateAliasResolution: a date param with default "today" resolves against
// the injected clock, and the upstream receives the canonical date.
func TestDateAliasResolution(t *testing.T) {
	var received string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = r.URL.Query().Get("date")
		_, _ = w.Write([]byte(`ok`))
	}))
	defer upstream.Close()

	cfg := &Config{
		Upstreams: map[string]Upstream{
			"demo": {
				Auth: AuthConfig{Type: "header", Headers: map[string]string{"X-T": "t"}},
				Endpoints: map[string]Endpoint{
					"things": {
						URL: upstream.URL,
						Params: map[string]Param{
							"date": {Type: ParamTypeDate, Default: "today"},
						},
					},
				},
			},
		},
	}
	p := newTestProxy(t, cfg)
	p.now = fixedClock("2026-04-22")
	srv := httptest.NewServer(p.buildMux())
	defer srv.Close()

	// Default applies.
	r1, _ := http.Get(srv.URL + "/demo/things")
	r1.Body.Close()
	if received != "2026-04-22" {
		t.Errorf("default today: got %q want 2026-04-22", received)
	}

	// Explicit alias.
	r2, _ := http.Get(srv.URL + "/demo/things?date=yesterday")
	r2.Body.Close()
	if received != "2026-04-21" {
		t.Errorf("yesterday: got %q want 2026-04-21", received)
	}

	// today-N.
	r3, _ := http.Get(srv.URL + "/demo/things?date=today-7")
	r3.Body.Close()
	if received != "2026-04-15" {
		t.Errorf("today-7: got %q want 2026-04-15", received)
	}
}

// TestDateAliasCacheKey: date=today and date=2026-04-22 share a cache entry
// when the clock is at the 22nd. Different days use different entries.
func TestDateAliasCacheKey(t *testing.T) {
	var hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write([]byte(`ok`))
	}))
	defer upstream.Close()

	cfg := &Config{
		Upstreams: map[string]Upstream{
			"demo": {
				Auth: AuthConfig{Type: "header", Headers: map[string]string{"X-T": "t"}},
				Endpoints: map[string]Endpoint{
					"things": {
						URL: upstream.URL,
						Params: map[string]Param{
							"date": {Type: ParamTypeDate},
						},
						MinInterval: time.Hour,
					},
				},
			},
		},
	}
	p := newTestProxy(t, cfg)
	p.now = fixedClock("2026-04-22")
	frozen := time.Unix(1_000_000, 0)
	p.throttle.now = func() time.Time { return frozen }

	srv := httptest.NewServer(p.buildMux())
	defer srv.Close()

	// Both of these should resolve to date=2026-04-22 and share a cache entry.
	r1, _ := http.Get(srv.URL + "/demo/things?date=today")
	r1.Body.Close()
	r2, _ := http.Get(srv.URL + "/demo/things?date=2026-04-22")
	r2.Body.Close()
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("today and 2026-04-22 should share cache; got %d upstream hits", got)
	}

	// A different date must not hit.
	r3, _ := http.Get(srv.URL + "/demo/things?date=2026-04-23")
	r3.Body.Close()
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Errorf("different date should be a cache miss; got %d upstream hits", got)
	}
}

// TestUnknownParamRejected: inbound params not in the endpoint schema must
// 400. This is the safety posture the old allowed_params allowlist provided.
func TestUnknownParamRejected(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`ok`))
	}))
	defer upstream.Close()

	cfg := &Config{
		Upstreams: map[string]Upstream{
			"demo": {
				Auth: AuthConfig{Type: "header", Headers: map[string]string{"X-T": "t"}},
				Endpoints: map[string]Endpoint{
					"things": {
						URL: upstream.URL,
						Params: map[string]Param{
							"date": {Type: ParamTypeDate},
						},
					},
				},
			},
		},
	}
	p := newTestProxy(t, cfg)
	p.now = fixedClock("2026-04-22")
	srv := httptest.NewServer(p.buildMux())
	defer srv.Close()

	r, _ := http.Get(srv.URL + "/demo/things?sneaky=1")
	r.Body.Close()
	if r.StatusCode != 400 {
		t.Errorf("unknown param should 400, got %d", r.StatusCode)
	}
}

// TestJQFilter: a projection filter strips extra fields from the cached body.
func TestJQFilter(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"a":1,"b":2,"c":3}`))
	}))
	defer upstream.Close()

	cfg := &Config{
		Upstreams: map[string]Upstream{
			"demo": {
				Auth: AuthConfig{Type: "header", Headers: map[string]string{"X-T": "t"}},
				Endpoints: map[string]Endpoint{
					"things": {
						URL:            upstream.URL,
						compiledFilter: mustCompileFilter(t, "{a: .a}"),
					},
				},
			},
		},
	}
	p := newTestProxy(t, cfg)
	srv := httptest.NewServer(p.buildMux())
	defer srv.Close()

	r, _ := http.Get(srv.URL + "/demo/things")
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()

	var parsed map[string]interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("response not JSON: %v; body=%s", err, body)
	}
	if _, ok := parsed["a"]; !ok {
		t.Errorf("response should keep field a; body=%s", body)
	}
	if _, ok := parsed["b"]; ok {
		t.Errorf("filter should have stripped b; body=%s", body)
	}
}

// TestJQFilterRuntimeError: a filter that errors at runtime must log and
// pass through the unfiltered body, not 500.
func TestJQFilterRuntimeError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// .name on an integer will error at runtime.
		_, _ = w.Write([]byte(`42`))
	}))
	defer upstream.Close()

	cfg := &Config{
		Upstreams: map[string]Upstream{
			"demo": {
				Auth: AuthConfig{Type: "header", Headers: map[string]string{"X-T": "t"}},
				Endpoints: map[string]Endpoint{
					"things": {
						URL:            upstream.URL,
						compiledFilter: mustCompileFilter(t, ".name"),
					},
				},
			},
		},
	}
	p := newTestProxy(t, cfg)
	srv := httptest.NewServer(p.buildMux())
	defer srv.Close()

	r, _ := http.Get(srv.URL + "/demo/things")
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Errorf("expected 200 on runtime filter error, got %d", r.StatusCode)
	}
	if string(body) != "42" {
		t.Errorf("expected unfiltered body %q, got %q", "42", body)
	}
}

// TestJQFilterMultiOutput: a filter with multiple outputs produces a JSON
// array.
func TestJQFilterMultiOutput(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[1,2,3]`))
	}))
	defer upstream.Close()

	cfg := &Config{
		Upstreams: map[string]Upstream{
			"demo": {
				Auth: AuthConfig{Type: "header", Headers: map[string]string{"X-T": "t"}},
				Endpoints: map[string]Endpoint{
					"things": {
						URL:            upstream.URL,
						compiledFilter: mustCompileFilter(t, ".[]"),
					},
				},
			},
		},
	}
	p := newTestProxy(t, cfg)
	srv := httptest.NewServer(p.buildMux())
	defer srv.Close()

	r, _ := http.Get(srv.URL + "/demo/things")
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if string(body) != "[1,2,3]" {
		t.Errorf("expected multi-output joined as array, got %q", body)
	}
}

// TestGraphQLReturns501: an endpoint with type graphql returns 501 and a
// message, without crashing.
func TestGraphQLReturns501(t *testing.T) {
	cfg := &Config{
		Upstreams: map[string]Upstream{
			"demo": {
				Auth: AuthConfig{Type: "header", Headers: map[string]string{"X-T": "t"}},
				Endpoints: map[string]Endpoint{
					"ql": {
						Type: EndpointTypeGraphQL,
						URL:  "https://example.com/graphql",
					},
				},
			},
		},
	}
	p := newTestProxy(t, cfg)
	srv := httptest.NewServer(p.buildMux())
	defer srv.Close()

	r, _ := http.Get(srv.URL + "/demo/ql")
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if r.StatusCode != 501 {
		t.Errorf("expected 501, got %d", r.StatusCode)
	}
	if !strings.Contains(string(body), "graphql") {
		t.Errorf("body should mention graphql; got %s", body)
	}
}

// TestManifestEndpoint: /_manifest returns configured endpoint metadata and
// excludes url/auth/filter/min_interval.
func TestManifestEndpoint(t *testing.T) {
	cfg := &Config{
		Upstreams: map[string]Upstream{
			"demo": {
				Auth: AuthConfig{
					Type:    "header",
					Headers: map[string]string{"Authorization": "Bearer secret"},
				},
				Endpoints: map[string]Endpoint{
					"things": {
						URL:         "https://example.com/api/things",
						Description: "The things endpoint",
						Params: map[string]Param{
							"date": {
								Type:        ParamTypeDate,
								Description: "date param",
								Default:     "today",
							},
						},
						Filter:      "{a: .a}",
						MinInterval: 30 * time.Second,
						MCP:         &MCPMeta{ToolName: "get_things"},
					},
					// An endpoint without an MCP block should still appear
					// in the manifest but with no mcp key.
					"internal": {
						URL: "https://example.com/api/internal",
					},
				},
			},
		},
	}
	p := newTestProxy(t, cfg)
	srv := httptest.NewServer(p.buildMux())
	defer srv.Close()

	r, _ := http.Get(srv.URL + "/_manifest")
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("expected 200, got %d; body=%s", r.StatusCode, body)
	}

	// Sensitive content must not appear anywhere.
	forbidden := []string{
		"https://example.com/api/things",
		"Bearer secret",
		"Authorization",
		"{a: .a}",
		"30s",
		"min_interval",
		"filter",
		"auth",
		"url",
	}
	lower := strings.ToLower(string(body))
	for _, bad := range forbidden {
		if strings.Contains(lower, strings.ToLower(bad)) {
			t.Errorf("manifest contains forbidden content %q: %s", bad, body)
		}
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("not JSON: %v; body=%s", err, body)
	}
	if parsed["version"] != "test" {
		t.Errorf("version: got %v", parsed["version"])
	}

	// Walk into the expected structure.
	ups, ok := parsed["upstreams"].(map[string]interface{})
	if !ok {
		t.Fatalf("upstreams missing/wrong type: %v", parsed)
	}
	demo, ok := ups["demo"].(map[string]interface{})
	if !ok {
		t.Fatalf("demo missing: %v", ups)
	}
	eps, ok := demo["endpoints"].(map[string]interface{})
	if !ok {
		t.Fatalf("endpoints missing: %v", demo)
	}
	things, ok := eps["things"].(map[string]interface{})
	if !ok {
		t.Fatalf("things missing: %v", eps)
	}
	if things["type"] != "rest" {
		t.Errorf("type: got %v", things["type"])
	}
	if things["description"] != "The things endpoint" {
		t.Errorf("description: got %v", things["description"])
	}
	params, ok := things["params"].(map[string]interface{})
	if !ok {
		t.Fatalf("params missing: %v", things)
	}
	date, ok := params["date"].(map[string]interface{})
	if !ok {
		t.Fatalf("date param missing")
	}
	if date["type"] != "date" {
		t.Errorf("param type: got %v", date["type"])
	}
	if date["default"] != "today" {
		t.Errorf("param default: got %v", date["default"])
	}
	if date["required"] != false {
		t.Errorf("param required: got %v", date["required"])
	}

	// mcp block present on the endpoint with an MCPMeta.
	mcp, ok := things["mcp"].(map[string]interface{})
	if !ok {
		t.Fatalf("mcp block missing on endpoint with MCPMeta: %v", things)
	}
	if mcp["tool_name"] != "get_things" {
		t.Errorf("mcp.tool_name: got %v", mcp["tool_name"])
	}

	// mcp block absent on the endpoint without an MCPMeta.
	internal, ok := eps["internal"].(map[string]interface{})
	if !ok {
		t.Fatalf("internal endpoint missing")
	}
	if _, has := internal["mcp"]; has {
		t.Errorf("mcp block should be omitted when MCPMeta is nil: %v", internal)
	}
}

// TestNoInboundAuth: with no auth configured at the application layer,
// every route is reachable without credentials. Security is the network
// boundary (ClusterIP, no ingress).
func TestNoInboundAuth(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cfg := &Config{
		Upstreams: map[string]Upstream{
			"demo": {
				Auth: AuthConfig{Type: "header", Headers: map[string]string{"X-T": "t"}},
				Endpoints: map[string]Endpoint{
					"things": {URL: upstream.URL, Params: map[string]Param{"date": {Type: ParamTypeDate}}},
				},
			},
		},
	}
	p := newTestProxy(t, cfg)
	p.now = fixedClock("2026-04-22")
	srv := httptest.NewServer(p.buildMux())
	defer srv.Close()

	for _, path := range []string{
		"/demo/things?date=2026-04-22",
		"/healthz",
		"/_manifest",
	} {
		r, _ := http.Get(srv.URL + path)
		r.Body.Close()
		if r.StatusCode != 200 {
			t.Errorf("%s: expected 200, got %d", path, r.StatusCode)
		}
	}
}

// TestMinIntervalFloor: inside the floor, upstream is not called; a second
// request returns the first's cached body. Outside the floor, upstream is
// called again.
func TestMinIntervalFloor(t *testing.T) {
	var hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"n":1}`))
	}))
	defer upstream.Close()

	cfg := &Config{
		Upstreams: map[string]Upstream{
			"demo": {
				Auth: AuthConfig{Type: "header", Headers: map[string]string{"X-T": "t"}},
				Endpoints: map[string]Endpoint{
					"things": {
						URL:         upstream.URL,
						Params:      map[string]Param{"date": {Type: ParamTypeDate}},
						MinInterval: time.Hour,
					},
				},
			},
		},
	}
	p := newTestProxy(t, cfg)
	p.now = fixedClock("2026-04-22")
	frozen := time.Unix(1_000_000, 0)
	p.throttle.now = func() time.Time { return frozen }

	srv := httptest.NewServer(p.buildMux())
	defer srv.Close()

	r1, _ := http.Get(srv.URL + "/demo/things?date=2026-04-22")
	b1, _ := io.ReadAll(r1.Body)
	r1.Body.Close()
	if atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("first request: expected 1 upstream hit, got %d", atomic.LoadInt32(&hits))
	}

	r2, _ := http.Get(srv.URL + "/demo/things?date=2026-04-22")
	b2, _ := io.ReadAll(r2.Body)
	r2.Body.Close()
	if atomic.LoadInt32(&hits) != 1 {
		t.Errorf("second (floor) request: expected still 1 upstream hit, got %d", atomic.LoadInt32(&hits))
	}
	if string(b1) != string(b2) {
		t.Errorf("cached response body differs: %q vs %q", b1, b2)
	}

	frozen = frozen.Add(2 * time.Hour)
	r3, _ := http.Get(srv.URL + "/demo/things?date=2026-04-22")
	r3.Body.Close()
	if atomic.LoadInt32(&hits) != 2 {
		t.Errorf("post-floor request: expected 2 upstream hits, got %d", atomic.LoadInt32(&hits))
	}
}

// TestFormLoginCookieReuse: first inbound triggers a login POST; subsequent
// inbounds reuse the session (no repeat login). The upstream API endpoint
// sees the cookie that was set during login.
func TestFormLoginCookieReuse(t *testing.T) {
	var loginHits, apiHits int32
	var lastAPICookie string

	mux := http.NewServeMux()
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&loginHits, 1)
		if err := r.ParseForm(); err != nil {
			t.Errorf("login parse form: %v", err)
		}
		if r.Form.Get("Email") != "u@example.com" || r.Form.Get("Password") != "p" {
			t.Errorf("login form mismatch: %v", r.Form)
		}
		http.SetCookie(w, &http.Cookie{Name: "SESSION", Value: "abc123", Path: "/"})
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/things", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&apiHits, 1)
		if c, err := r.Cookie("SESSION"); err == nil {
			lastAPICookie = c.Value
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	upstream := httptest.NewServer(mux)
	defer upstream.Close()

	cfg := &Config{
		Upstreams: map[string]Upstream{
			"demo": {
				Auth: AuthConfig{
					Type:     "form_login",
					LoginURL: upstream.URL + "/login",
					FormFields: map[string]string{
						"Email":    "u@example.com",
						"Password": "p",
					},
				},
				Endpoints: map[string]Endpoint{
					"things": {URL: upstream.URL + "/api/things", Params: map[string]Param{"date": {Type: ParamTypeDate}}},
				},
			},
		},
	}
	p := newTestProxy(t, cfg)
	p.now = fixedClock("2026-04-22")
	srv := httptest.NewServer(p.buildMux())
	defer srv.Close()

	for i := 0; i < 3; i++ {
		r, err := http.Get(srv.URL + "/demo/things?date=2026-04-22")
		if err != nil {
			t.Fatalf("GET %d: %v", i, err)
		}
		r.Body.Close()
	}

	if got := atomic.LoadInt32(&loginHits); got != 1 {
		t.Errorf("expected 1 login, got %d (should reuse session)", got)
	}
	if got := atomic.LoadInt32(&apiHits); got != 3 {
		t.Errorf("expected 3 api hits, got %d", got)
	}
	if lastAPICookie != "abc123" {
		t.Errorf("upstream did not see reused cookie, got %q", lastAPICookie)
	}
}

// TestFormLoginReloginOnAuthFailure: first inbound succeeds, second returns
// 401 from upstream, the proxy should re-login and retry once.
func TestFormLoginReloginOnAuthFailure(t *testing.T) {
	var loginHits int32
	var requestCount int32

	mux := http.NewServeMux()
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&loginHits, 1)
		http.SetCookie(w, &http.Cookie{Name: "SESSION", Value: "v", Path: "/"})
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/things", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&requestCount, 1)
		if n == 2 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"session expired"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	upstream := httptest.NewServer(mux)
	defer upstream.Close()

	cfg := &Config{
		Upstreams: map[string]Upstream{
			"demo": {
				Auth: AuthConfig{
					Type:       "form_login",
					LoginURL:   upstream.URL + "/login",
					FormFields: map[string]string{"Email": "u", "Password": "p"},
				},
				Endpoints: map[string]Endpoint{
					"things": {URL: upstream.URL + "/api/things", Params: map[string]Param{"date": {Type: ParamTypeString}}},
				},
			},
		},
	}
	p := newTestProxy(t, cfg)
	srv := httptest.NewServer(p.buildMux())
	defer srv.Close()

	r1, _ := http.Get(srv.URL + "/demo/things?date=1")
	r1.Body.Close()
	r2, _ := http.Get(srv.URL + "/demo/things?date=2")
	b2, _ := io.ReadAll(r2.Body)
	r2.Body.Close()

	if got := atomic.LoadInt32(&loginHits); got != 2 {
		t.Errorf("expected 2 logins (initial + relogin), got %d", got)
	}
	if r2.StatusCode != 200 {
		t.Errorf("second request final status: expected 200, got %d; body=%s", r2.StatusCode, b2)
	}
	if got := atomic.LoadInt32(&requestCount); got != 3 {
		t.Errorf("expected 3 upstream api requests, got %d", got)
	}
}

// TestALB403Retry: upstream returns a bare 403 (no body) on first attempt,
// 200 on second.
func TestALB403Retry(t *testing.T) {
	var n int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) == 1 {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cfg := &Config{
		Upstreams: map[string]Upstream{
			"demo": {
				Auth: AuthConfig{Type: "header", Headers: map[string]string{"X-T": "t"}},
				Endpoints: map[string]Endpoint{
					"things": {URL: upstream.URL, Params: map[string]Param{"date": {Type: ParamTypeDate}}},
				},
			},
		},
	}
	p := newTestProxy(t, cfg)
	p.now = fixedClock("2026-04-22")
	srv := httptest.NewServer(p.buildMux())
	defer srv.Close()

	start := time.Now()
	r, err := http.Get(srv.URL + "/demo/things?date=2026-04-22")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()

	if r.StatusCode != 200 {
		t.Errorf("expected 200 after ALB retry, got %d; body=%s", r.StatusCode, body)
	}
	if atomic.LoadInt32(&n) != 2 {
		t.Errorf("expected 2 upstream attempts, got %d", n)
	}
	if elapsed := time.Since(start); elapsed < 500*time.Millisecond {
		t.Errorf("expected at least ~1s pause before retry, got %v", elapsed)
	}
	if !strings.Contains(string(body), `"ok":true`) {
		t.Errorf("unexpected body: %s", body)
	}
}

// TestFormLoginBare403IsALBRace: form_login upstream returns a bare 403 —
// treated as ALB stickiness race, not auth failure, so retry without re-login.
func TestFormLoginBare403IsALBRace(t *testing.T) {
	var loginHits, apiHits int32
	mux := http.NewServeMux()
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&loginHits, 1)
		http.SetCookie(w, &http.Cookie{Name: "SESSION", Value: "v", Path: "/"})
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/things", func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&apiHits, 1) == 1 {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	upstream := httptest.NewServer(mux)
	defer upstream.Close()

	cfg := &Config{
		Upstreams: map[string]Upstream{
			"demo": {
				Auth: AuthConfig{
					Type:       "form_login",
					LoginURL:   upstream.URL + "/login",
					FormFields: map[string]string{"Email": "u", "Password": "p"},
				},
				Endpoints: map[string]Endpoint{
					"things": {URL: upstream.URL + "/api/things", Params: map[string]Param{"date": {Type: ParamTypeDate}}},
				},
			},
		},
	}
	p := newTestProxy(t, cfg)
	p.now = fixedClock("2026-04-22")
	srv := httptest.NewServer(p.buildMux())
	defer srv.Close()

	r, _ := http.Get(srv.URL + "/demo/things?date=2026-04-22")
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Errorf("expected 200, got %d", r.StatusCode)
	}
	if got := atomic.LoadInt32(&loginHits); got != 1 {
		t.Errorf("bare 403 should not trigger re-login; logins=%d", got)
	}
	if got := atomic.LoadInt32(&apiHits); got != 2 {
		t.Errorf("expected 2 api hits (initial 403 + retry), got %d", got)
	}
}

// TestConcurrentLoginStampede: N inbound requests arriving simultaneously
// against a fresh proxy must trigger exactly one login, not N.
func TestConcurrentLoginStampede(t *testing.T) {
	var loginHits int32
	mux := http.NewServeMux()
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&loginHits, 1)
		time.Sleep(50 * time.Millisecond)
		http.SetCookie(w, &http.Cookie{Name: "SESSION", Value: "v", Path: "/"})
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/things", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	upstream := httptest.NewServer(mux)
	defer upstream.Close()

	cfg := &Config{
		Upstreams: map[string]Upstream{
			"demo": {
				Auth: AuthConfig{
					Type:       "form_login",
					LoginURL:   upstream.URL + "/login",
					FormFields: map[string]string{"Email": "u", "Password": "p"},
				},
				Endpoints: map[string]Endpoint{
					"things": {URL: upstream.URL + "/api/things", Params: map[string]Param{"date": {Type: ParamTypeString}}},
				},
			},
		},
	}
	p := newTestProxy(t, cfg)
	srv := httptest.NewServer(p.buildMux())
	defer srv.Close()

	const N = 20
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(i int) {
			defer wg.Done()
			r, err := http.Get(srv.URL + "/demo/things?date=2026-04-2" + strconv.Itoa(i%10))
			if err != nil {
				t.Errorf("GET %d: %v", i, err)
				return
			}
			r.Body.Close()
			if r.StatusCode != 200 {
				t.Errorf("GET %d status: %d", i, r.StatusCode)
			}
		}(i)
	}
	wg.Wait()

	if got := atomic.LoadInt32(&loginHits); got != 1 {
		t.Errorf("expected exactly 1 login under concurrent inbound; got %d", got)
	}
}

// TestHeaderAuthSendsHeaders: header auth type includes headers on every
// outbound request.
func TestHeaderAuthSendsHeaders(t *testing.T) {
	var got string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`ok`))
	}))
	defer upstream.Close()

	cfg := &Config{
		Upstreams: map[string]Upstream{
			"demo": {
				Auth: AuthConfig{Type: "header", Headers: map[string]string{"Authorization": "Bearer xyz"}},
				Endpoints: map[string]Endpoint{
					"things": {URL: upstream.URL, Params: map[string]Param{"date": {Type: ParamTypeDate}}},
				},
			},
		},
	}
	p := newTestProxy(t, cfg)
	p.now = fixedClock("2026-04-22")
	srv := httptest.NewServer(p.buildMux())
	defer srv.Close()

	r, _ := http.Get(srv.URL + "/demo/things?date=2026-04-22")
	r.Body.Close()
	if got != "Bearer xyz" {
		t.Errorf("upstream did not receive Authorization header; got %q", got)
	}
}

// TestUserAgentHonest: every outbound request carries the honest User-Agent.
func TestUserAgentHonest(t *testing.T) {
	var got string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("User-Agent")
		_, _ = w.Write([]byte(`ok`))
	}))
	defer upstream.Close()

	cfg := &Config{
		Upstreams: map[string]Upstream{
			"demo": {
				Auth: AuthConfig{Type: "header", Headers: map[string]string{"X-K": "v"}},
				Endpoints: map[string]Endpoint{
					"things": {URL: upstream.URL, Params: map[string]Param{"date": {Type: ParamTypeDate}}},
				},
			},
		},
	}
	p := newTestProxy(t, cfg)
	p.now = fixedClock("2026-04-22")
	srv := httptest.NewServer(p.buildMux())
	defer srv.Close()

	r, _ := http.Get(srv.URL + "/demo/things?date=2026-04-22")
	r.Body.Close()
	if !strings.HasPrefix(got, "vestibule/") {
		t.Errorf("User-Agent not honest: %q", got)
	}
	if !strings.Contains(got, "+https://github.com/zuzak/vestibule") {
		t.Errorf("User-Agent missing informational URL: %q", got)
	}
}

// TestUnknownUpstreamAndEndpoint: 404 with useful body.
func TestUnknownPaths(t *testing.T) {
	cfg := &Config{
		Upstreams: map[string]Upstream{
			"demo": {
				Auth: AuthConfig{Type: "header", Headers: map[string]string{"X-K": "v"}},
				Endpoints: map[string]Endpoint{
					"things": {URL: "http://127.0.0.1:1"},
				},
			},
		},
	}
	p := newTestProxy(t, cfg)
	srv := httptest.NewServer(p.buildMux())
	defer srv.Close()

	cases := []struct {
		path   string
		status int
	}{
		{"/ghost/things", 404},
		{"/demo/unknown", 404},
		{"/", 404},
		{"/demo", 404},
	}
	for _, tc := range cases {
		r, _ := http.Get(srv.URL + tc.path)
		r.Body.Close()
		if r.StatusCode != tc.status {
			t.Errorf("%s: expected %d, got %d", tc.path, tc.status, r.StatusCode)
		}
	}
}
