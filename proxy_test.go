package main

import (
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

	"github.com/prometheus/client_golang/prometheus"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testMetrics() *metrics {
	return newMetrics(prometheus.NewRegistry())
}

// newTestProxy spins up an http.Client against a httptest.Server without any
// timeout override beyond the default. Tests that need to assert against
// concrete state (loggedIn, loginMu) use the returned *proxy directly.
func newTestProxy(t *testing.T, cfg *Config) *proxy {
	t.Helper()
	p, err := newProxy(cfg, testLogger(), testMetrics(), "test")
	if err != nil {
		t.Fatalf("newProxy: %v", err)
	}
	return p
}

// TestParamAllowlist covers: allowed param passes through, disallowed param
// returns 400, apikey is consumed and never forwarded.
func TestParamAllowlist(t *testing.T) {
	var receivedQuery url.Values
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedQuery = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cfg := &Config{
		APIKey: "k",
		Upstreams: map[string]Upstream{
			"demo": {
				Auth: AuthConfig{Type: "header", Headers: map[string]string{"X-Token": "t"}},
				Endpoints: map[string]Endpoint{
					"things": {URL: upstream.URL, AllowedParams: []string{"date"}},
				},
			},
		},
	}
	p := newTestProxy(t, cfg)
	srv := httptest.NewServer(p.buildMux())
	defer srv.Close()

	// Allowed param passes through.
	resp, err := http.Get(srv.URL + "/demo/things?apikey=k&date=2026-04-22")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	if got := receivedQuery.Get("date"); got != "2026-04-22" {
		t.Errorf("upstream received date=%q, want 2026-04-22", got)
	}
	if receivedQuery.Get("apikey") != "" {
		t.Errorf("apikey leaked to upstream: %q", receivedQuery.Get("apikey"))
	}

	// Disallowed param returns 400.
	resp2, err := http.Get(srv.URL + "/demo/things?apikey=k&sneaky=1")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != 400 {
		t.Errorf("expected 400, got %d; body=%s", resp2.StatusCode, body)
	}
}

// TestInboundAPIKey covers: missing/wrong key → 401, correct key → 200,
// /healthz always 200 regardless of key.
func TestInboundAPIKey(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cfg := &Config{
		APIKey: "secret",
		Upstreams: map[string]Upstream{
			"demo": {
				Auth:      AuthConfig{Type: "header", Headers: map[string]string{"X-Token": "t"}},
				Endpoints: map[string]Endpoint{"things": {URL: upstream.URL, AllowedParams: []string{"date"}}},
			},
		},
	}
	p := newTestProxy(t, cfg)
	srv := httptest.NewServer(p.buildMux())
	defer srv.Close()

	missing, _ := http.Get(srv.URL + "/demo/things?date=2026-04-22")
	missing.Body.Close()
	if missing.StatusCode != 401 {
		t.Errorf("missing key: expected 401, got %d", missing.StatusCode)
	}

	wrong, _ := http.Get(srv.URL + "/demo/things?apikey=wrong&date=2026-04-22")
	wrong.Body.Close()
	if wrong.StatusCode != 401 {
		t.Errorf("wrong key: expected 401, got %d", wrong.StatusCode)
	}

	right, _ := http.Get(srv.URL + "/demo/things?apikey=secret&date=2026-04-22")
	right.Body.Close()
	if right.StatusCode != 200 {
		t.Errorf("correct key: expected 200, got %d", right.StatusCode)
	}

	health, _ := http.Get(srv.URL + "/healthz")
	health.Body.Close()
	if health.StatusCode != 200 {
		t.Errorf("/healthz with no key: expected 200, got %d", health.StatusCode)
	}
}

// TestInboundAPIKeyUnconfigured: if api_key is empty, no auth check runs.
func TestInboundAPIKeyUnconfigured(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`ok`))
	}))
	defer upstream.Close()

	cfg := &Config{
		Upstreams: map[string]Upstream{
			"demo": {
				Auth:      AuthConfig{Type: "header", Headers: map[string]string{"X-Token": "t"}},
				Endpoints: map[string]Endpoint{"things": {URL: upstream.URL, AllowedParams: []string{"date"}}},
			},
		},
	}
	p := newTestProxy(t, cfg)
	srv := httptest.NewServer(p.buildMux())
	defer srv.Close()

	resp, _ := http.Get(srv.URL + "/demo/things?date=2026-04-22")
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("expected 200 when api_key unset, got %d", resp.StatusCode)
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
				Auth: AuthConfig{Type: "header", Headers: map[string]string{"X-Token": "t"}},
				Endpoints: map[string]Endpoint{
					"things": {URL: upstream.URL, AllowedParams: []string{"date"}, MinInterval: time.Hour},
				},
			},
		},
	}
	p := newTestProxy(t, cfg)
	// Control throttle time so we can step past the floor.
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
					"things": {URL: upstream.URL + "/api/things", AllowedParams: []string{"date"}},
				},
			},
		},
	}
	p := newTestProxy(t, cfg)
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
		// Requests: 1 = OK (initial), 2 = 401 (session expired), 3 = OK (after relogin)
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
					"things": {URL: upstream.URL + "/api/things", AllowedParams: []string{"date"}},
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
	// Request count: 1 (initial OK) + 1 (401) + 1 (retry OK) = 3.
	if got := atomic.LoadInt32(&requestCount); got != 3 {
		t.Errorf("expected 3 upstream api requests, got %d", got)
	}
}

// TestALB403Retry: upstream returns a bare 403 (no body) on first attempt,
// 200 on second. The proxy should retry once after a brief pause and serve
// the 200. This must work for both auth types; test with `header`.
func TestALB403Retry(t *testing.T) {
	var n int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) == 1 {
			// Bare 403 — no body.
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
				Auth:      AuthConfig{Type: "header", Headers: map[string]string{"X-Token": "t"}},
				Endpoints: map[string]Endpoint{"things": {URL: upstream.URL, AllowedParams: []string{"date"}}},
			},
		},
	}
	// Override the retry delay by cheating on the package-level constant is
	// not possible; instead we accept the ~1s pause in the test. Keep the
	// test fast by making only one request.
	p := newTestProxy(t, cfg)
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

// TestBareForbiddenFromFormLoginOnlyRetriesALBNotRelogin: form_login upstream
// returns a bare 403 — this is treated as the ALB stickiness race, not an
// auth failure, so we retry without a re-login.
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
			// Bare 403 on first call — ALB race.
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
					"things": {URL: upstream.URL + "/api/things", AllowedParams: []string{"date"}},
				},
			},
		},
	}
	p := newTestProxy(t, cfg)
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
// against a fresh proxy must trigger exactly one login, not N. The per-
// upstream login mutex serialises the auth flow.
func TestConcurrentLoginStampede(t *testing.T) {
	var loginHits int32
	mux := http.NewServeMux()
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&loginHits, 1)
		// A slow login amplifies the race window; without the mutex, every
		// caller gets through `loggedIn == false` before the first one
		// returns, and they all attempt to log in.
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
					"things": {URL: upstream.URL + "/api/things", AllowedParams: []string{"date"}},
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
				Auth:      AuthConfig{Type: "header", Headers: map[string]string{"Authorization": "Bearer xyz"}},
				Endpoints: map[string]Endpoint{"things": {URL: upstream.URL, AllowedParams: []string{"date"}}},
			},
		},
	}
	p := newTestProxy(t, cfg)
	srv := httptest.NewServer(p.buildMux())
	defer srv.Close()

	r, _ := http.Get(srv.URL + "/demo/things?date=2026-04-22")
	r.Body.Close()
	if got != "Bearer xyz" {
		t.Errorf("upstream did not receive Authorization header; got %q", got)
	}
}

// TestUserAgentHonest: every outbound request carries the honest
// vestibule/<version> User-Agent.
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
				Auth:      AuthConfig{Type: "header", Headers: map[string]string{"X-K": "v"}},
				Endpoints: map[string]Endpoint{"things": {URL: upstream.URL, AllowedParams: []string{"date"}}},
			},
		},
	}
	p := newTestProxy(t, cfg)
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
				Auth:      AuthConfig{Type: "header", Headers: map[string]string{"X-K": "v"}},
				Endpoints: map[string]Endpoint{"things": {URL: "http://127.0.0.1:1", AllowedParams: []string{}}},
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
