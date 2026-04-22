package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// upstreamRuntime holds the per-upstream mutable state: cookie jar, HTTP
// client, and a login mutex so that concurrent session recoveries do not
// fan out into N parallel login attempts against the upstream.
type upstreamRuntime struct {
	name     string
	cfg      Upstream
	client   *http.Client
	loginMu  sync.Mutex
	loggedIn bool // only meaningful for form_login
}

type proxy struct {
	cfg       *Config
	logger    *slog.Logger
	metrics   *metrics
	throttle  *throttle
	upstreams map[string]*upstreamRuntime
	userAgent string
}

// upstreamTimeout bounds every outbound HTTP call. An upstream with no
// timeout can hang a goroutine forever.
const upstreamTimeout = 30 * time.Second

// albRetryDelay is the pause between the initial bare-403 and the retry. ALB
// stickiness rotations resolve in well under a second in practice; 1s is a
// comfortable margin without being user-visibly slow.
const albRetryDelay = 1 * time.Second

func newProxy(cfg *Config, logger *slog.Logger, m *metrics, version string) (*proxy, error) {
	p := &proxy{
		cfg:       cfg,
		logger:    logger,
		metrics:   m,
		throttle:  newThrottle(),
		upstreams: make(map[string]*upstreamRuntime),
		userAgent: fmt.Sprintf("vestibule/%s (+https://github.com/zuzak/vestibule)", version),
	}
	for name, up := range cfg.Upstreams {
		jar, err := cookiejar.New(nil)
		if err != nil {
			return nil, fmt.Errorf("cookiejar for %q: %w", name, err)
		}
		p.upstreams[name] = &upstreamRuntime{
			name:   name,
			cfg:    up,
			client: &http.Client{Jar: jar, Timeout: upstreamTimeout},
		}
	}
	return p, nil
}

// buildMux returns the public HTTP handler tree. /healthz is registered
// before the auth wrapper so healthchecks are never rejected.
func (p *proxy) buildMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("/", p.handleRequest)

	return p.withLogging(p.withAPIKey(mux))
}

// withAPIKey rejects requests missing or mismatching the configured key.
// /healthz is handled inside the wrapped mux but is never checked because we
// short-circuit on the exact path first.
func (p *proxy) withAPIKey(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		if p.cfg.APIKey == "" {
			next.ServeHTTP(w, r)
			return
		}
		if r.URL.Query().Get("apikey") != p.cfg.APIKey {
			writeJSONError(w, http.StatusUnauthorized, "invalid or missing apikey")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// withLogging records the inbound request line, final status, and duration.
// Never logs the query string — it may contain the apikey.
func (p *proxy) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		p.logger.Info("inbound",
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"duration_ms", time.Since(start).Milliseconds(),
			"remote", r.RemoteAddr,
		)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// handleRequest routes /<upstream>/<endpoint> and rejects anything else with
// 404. The root path returns a small JSON note so curl gets a useful response.
func (p *proxy) handleRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "only GET is supported")
		return
	}
	path := strings.Trim(r.URL.Path, "/")
	if path == "" {
		writeJSONError(w, http.StatusNotFound, "specify /<upstream>/<endpoint>")
		return
	}
	parts := strings.SplitN(path, "/", 2)
	if len(parts) != 2 || parts[1] == "" {
		writeJSONError(w, http.StatusNotFound, "specify /<upstream>/<endpoint>")
		return
	}
	upstreamName, endpointName := parts[0], parts[1]

	up, ok := p.upstreams[upstreamName]
	if !ok {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("unknown upstream %q", upstreamName))
		p.metrics.inbound.WithLabelValues(upstreamName, endpointName, "404").Inc()
		return
	}
	ep, ok := up.cfg.Endpoints[endpointName]
	if !ok {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("unknown endpoint %q for upstream %q", endpointName, upstreamName))
		p.metrics.inbound.WithLabelValues(upstreamName, endpointName, "404").Inc()
		return
	}

	params, err := filterParams(r.URL.Query(), ep.AllowedParams)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		p.metrics.inbound.WithLabelValues(upstreamName, endpointName, "400").Inc()
		return
	}

	key := canonicalKey(upstreamName, endpointName, params)
	if cached, hit := p.throttle.check(key, ep.MinInterval); hit {
		p.logger.Info("serving from min_interval floor",
			"upstream", upstreamName, "endpoint", endpointName)
		p.metrics.minIntervalHits.WithLabelValues(upstreamName, endpointName).Inc()
		writeCached(w, cached)
		p.metrics.inbound.WithLabelValues(upstreamName, endpointName, strconv.Itoa(cached.status)).Inc()
		return
	}

	result, err := p.fetchUpstream(r.Context(), up, endpointName, ep, params)
	if err != nil {
		p.logger.Error("upstream fetch failed",
			"upstream", upstreamName, "endpoint", endpointName, "error", err.Error())
		writeJSONError(w, http.StatusBadGateway, fmt.Sprintf("upstream request failed: %s", err.Error()))
		p.metrics.inbound.WithLabelValues(upstreamName, endpointName, "502").Inc()
		return
	}

	p.throttle.store(key, result)
	writeCached(w, result)
	p.metrics.inbound.WithLabelValues(upstreamName, endpointName, strconv.Itoa(result.status)).Inc()
}

func filterParams(got url.Values, allowed []string) (url.Values, error) {
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		allowedSet[name] = struct{}{}
	}
	// apikey is consumed by the inbound auth wrapper; never forwarded.
	out := url.Values{}
	for k, v := range got {
		if k == "apikey" {
			continue
		}
		if _, ok := allowedSet[k]; !ok {
			return nil, fmt.Errorf("param %q is not in allowed_params", k)
		}
		out[k] = v
	}
	return out, nil
}

// fetchUpstream performs the outbound request, applying auth, retry on 401/403
// (form_login only), and the ALB-bare-403 retry that applies to any upstream.
func (p *proxy) fetchUpstream(ctx context.Context, up *upstreamRuntime, endpointName string, ep Endpoint, params url.Values) (*cachedResponse, error) {
	// Attempt 1: ensure we have a session (form_login only), send the request.
	if up.cfg.Auth.Type == "form_login" {
		if err := p.ensureLoggedIn(ctx, up, false); err != nil {
			return nil, fmt.Errorf("login: %w", err)
		}
	}
	result, err := p.doUpstreamRequest(ctx, up, endpointName, ep, params)
	if err != nil {
		return nil, err
	}

	// ALB 403 race: bare 403 with no body. Retry once after a brief pause.
	if isBareForbidden(result) {
		p.logger.Info("ALB bare-403 detected, retrying once",
			"upstream", up.name, "endpoint", endpointName)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(albRetryDelay):
		}
		result, err = p.doUpstreamRequest(ctx, up, endpointName, ep, params)
		if err != nil {
			return nil, err
		}
	}

	// Session expiry: form_login only. A 401/403 with a real body is treated
	// as session-expired; attempt one re-login and retry.
	if up.cfg.Auth.Type == "form_login" && (result.status == http.StatusUnauthorized || result.status == http.StatusForbidden) {
		p.logger.Info("upstream rejected request, re-logging in",
			"upstream", up.name, "endpoint", endpointName, "status", result.status)
		if err := p.ensureLoggedIn(ctx, up, true); err != nil {
			return nil, fmt.Errorf("re-login failed: %w", err)
		}
		result, err = p.doUpstreamRequest(ctx, up, endpointName, ep, params)
		if err != nil {
			return nil, err
		}
	}

	return result, nil
}

func isBareForbidden(r *cachedResponse) bool {
	if r.status != http.StatusForbidden {
		return false
	}
	return len(r.body) == 0
}

func (p *proxy) doUpstreamRequest(ctx context.Context, up *upstreamRuntime, endpointName string, ep Endpoint, params url.Values) (*cachedResponse, error) {
	u, err := url.Parse(ep.URL)
	if err != nil {
		return nil, fmt.Errorf("parse endpoint url: %w", err)
	}
	q := u.Query()
	for k, vs := range params {
		for _, v := range vs {
			q.Add(k, v)
		}
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", p.userAgent)
	req.Header.Set("Accept", "application/json")

	if up.cfg.Auth.Type == "header" {
		for k, v := range up.cfg.Auth.Headers {
			req.Header.Set(k, v)
		}
	}

	start := time.Now()
	resp, err := up.client.Do(req)
	duration := time.Since(start)
	if err != nil {
		p.metrics.upstream.WithLabelValues(up.name, endpointName, "error").Inc()
		return nil, fmt.Errorf("upstream request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		p.metrics.upstream.WithLabelValues(up.name, endpointName, "error").Inc()
		return nil, fmt.Errorf("read upstream body: %w", err)
	}

	p.metrics.upstream.WithLabelValues(up.name, endpointName, strconv.Itoa(resp.StatusCode)).Inc()
	p.metrics.upstreamDuration.WithLabelValues(up.name, endpointName).Observe(duration.Seconds())

	p.logger.Info("upstream",
		"upstream", up.name,
		"endpoint", endpointName,
		"host", u.Host,
		"path", u.Path,
		"status", resp.StatusCode,
		"duration_ms", duration.Milliseconds(),
	)

	return &cachedResponse{
		status:  resp.StatusCode,
		headers: captureResponseHeaders(resp.Header),
		body:    body,
	}, nil
}

// captureResponseHeaders copies only headers that are safe and useful to
// forward. Cookies and hop-by-hop headers are dropped deliberately.
func captureResponseHeaders(h http.Header) map[string][]string {
	out := make(map[string][]string)
	for _, name := range []string{"Content-Type", "Content-Encoding", "Content-Language"} {
		if v := h.Values(name); len(v) > 0 {
			out[name] = v
		}
	}
	return out
}

// ensureLoggedIn runs the form_login flow. force=true bypasses the cached
// "logged in" flag and re-authenticates unconditionally. The login mutex
// ensures concurrent callers do not stampede the login endpoint.
func (p *proxy) ensureLoggedIn(ctx context.Context, up *upstreamRuntime, force bool) error {
	up.loginMu.Lock()
	defer up.loginMu.Unlock()

	if up.loggedIn && !force {
		return nil
	}

	method := up.cfg.Auth.LoginMethod
	if method == "" {
		method = http.MethodPost
	}

	form := url.Values{}
	for k, v := range up.cfg.Auth.FormFields {
		form.Set(k, v)
	}

	req, err := http.NewRequestWithContext(ctx, method, up.cfg.Auth.LoginURL, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("build login request: %w", err)
	}
	req.Header.Set("User-Agent", p.userAgent)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := up.client.Do(req)
	if err != nil {
		return fmt.Errorf("login request: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	// Many form-login endpoints redirect on success and return 4xx on failure.
	// Accept 2xx and 3xx. The real signal for form_login is cookie issuance,
	// but we cannot inspect the jar's contents per-upstream without
	// fragile URL-matching; trust the status line and let the subsequent
	// upstream request surface genuine failures.
	if resp.StatusCode >= 400 {
		return fmt.Errorf("login returned status %d", resp.StatusCode)
	}

	up.loggedIn = true
	return nil
}

func writeCached(w http.ResponseWriter, r *cachedResponse) {
	for k, vs := range r.headers {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(r.status)
	_, _ = w.Write(r.body)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
