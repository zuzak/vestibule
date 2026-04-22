package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTempConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	return path
}

func TestLoadConfigEnvInterpolation(t *testing.T) {
	t.Setenv("VEST_TEST_EMAIL", "user@example.com")
	t.Setenv("VEST_TEST_PASS", "s3cret")

	path := writeTempConfig(t, `
listen: ":8080"
api_key: ${VEST_TEST_UNSET}
upstreams:
  demo:
    auth:
      type: form_login
      login_url: https://example.com/login
      form_fields:
        Email: ${VEST_TEST_EMAIL}
        Password: ${VEST_TEST_PASS}
    endpoints:
      things:
        url: https://example.com/api/things
        params:
          date:
            type: date
            default: today
        min_interval: 30s
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.APIKey != "" {
		t.Errorf("expected unset ${VEST_TEST_UNSET} to interpolate to empty, got %q", cfg.APIKey)
	}
	up, ok := cfg.Upstreams["demo"]
	if !ok {
		t.Fatalf("upstream demo missing")
	}
	if up.Auth.FormFields["Email"] != "user@example.com" {
		t.Errorf("email interpolation: got %q", up.Auth.FormFields["Email"])
	}
	if up.Auth.FormFields["Password"] != "s3cret" {
		t.Errorf("password interpolation: got %q", up.Auth.FormFields["Password"])
	}
	if up.Endpoints["things"].MinInterval != 30*time.Second {
		t.Errorf("min_interval parse: got %v", up.Endpoints["things"].MinInterval)
	}
	p, ok := up.Endpoints["things"].Params["date"]
	if !ok {
		t.Fatalf("date param missing")
	}
	if p.Type != ParamTypeDate || p.Default != "today" {
		t.Errorf("param parse: type=%q default=%q", p.Type, p.Default)
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	path := writeTempConfig(t, `
upstreams:
  demo:
    auth:
      type: header
      headers:
        Authorization: Bearer xyz
    endpoints:
      things:
        url: https://example.com/api/things
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Listen != ":8080" {
		t.Errorf("default listen: got %q", cfg.Listen)
	}
	if cfg.MetricsListen != ":9090" {
		t.Errorf("default metrics_listen: got %q", cfg.MetricsListen)
	}
	ep := cfg.Upstreams["demo"].Endpoints["things"]
	if ep.endpointType() != EndpointTypeREST {
		t.Errorf("endpoint type default: got %q", ep.endpointType())
	}
}

func TestLoadConfigValidation(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "no upstreams",
			body: "listen: :8080\n",
			want: "no upstreams",
		},
		{
			name: "unsupported auth type",
			body: `
upstreams:
  demo:
    auth:
      type: oauth2
    endpoints:
      things:
        url: https://example.com/api/things
`,
			want: "unsupported auth type",
		},
		{
			name: "form_login missing url",
			body: `
upstreams:
  demo:
    auth:
      type: form_login
      form_fields: {Email: a}
    endpoints:
      things:
        url: https://example.com/api/things
`,
			want: "requires login_url",
		},
		{
			name: "header missing headers",
			body: `
upstreams:
  demo:
    auth:
      type: header
    endpoints:
      things:
        url: https://example.com/api/things
`,
			want: "requires headers",
		},
		{
			name: "endpoint missing url",
			body: `
upstreams:
  demo:
    auth:
      type: header
      headers: {X-Key: v}
    endpoints:
      things:
        params:
          date: {type: date}
`,
			want: "url is required",
		},
		{
			name: "unsupported endpoint type",
			body: `
upstreams:
  demo:
    auth:
      type: header
      headers: {X-Key: v}
    endpoints:
      things:
        url: https://example.com/api/things
        type: soap
`,
			want: "unsupported type",
		},
		{
			name: "unsupported param type",
			body: `
upstreams:
  demo:
    auth:
      type: header
      headers: {X-Key: v}
    endpoints:
      things:
        url: https://example.com/api/things
        params:
          weird: {type: uuid}
`,
			want: "unsupported type",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTempConfig(t, tc.body)
			_, err := LoadConfig(path)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

// TestRequiredAndDefaultRejected: a param declared both required and with a
// non-empty default must fail config load.
func TestRequiredAndDefaultRejected(t *testing.T) {
	path := writeTempConfig(t, `
upstreams:
  demo:
    auth:
      type: header
      headers: {X-K: v}
    endpoints:
      things:
        url: https://example.com/api/things
        params:
          date:
            type: date
            required: true
            default: today
`)
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error %q should mention mutually exclusive", err.Error())
	}
}

// TestJQFilterCompilesAtLoad: an invalid jq filter in config must fail load
// rather than surface at request time.
func TestJQFilterCompilesAtLoad(t *testing.T) {
	path := writeTempConfig(t, `
upstreams:
  demo:
    auth:
      type: header
      headers: {X-K: v}
    endpoints:
      things:
        url: https://example.com/api/things
        filter: "{broken"
`)
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatalf("expected parse error for invalid jq, got nil")
	}
	if !strings.Contains(err.Error(), "filter") {
		t.Errorf("error %q should mention filter", err.Error())
	}
}

// TestJQFilterCompilesGoodFilter: a valid jq filter compiles and produces a
// ready-to-use compiledFilter on the endpoint.
func TestJQFilterCompilesGoodFilter(t *testing.T) {
	path := writeTempConfig(t, `
upstreams:
  demo:
    auth:
      type: header
      headers: {X-K: v}
    endpoints:
      things:
        url: https://example.com/api/things
        filter: "{a: .a}"
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	ep := cfg.Upstreams["demo"].Endpoints["things"]
	if ep.compiledFilter == nil {
		t.Fatal("expected compiledFilter to be populated")
	}
}
