package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/itchyny/gojq"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen        string              `yaml:"listen"`
	MetricsListen string              `yaml:"metrics_listen"`
	APIKey        string              `yaml:"api_key"`
	Upstreams     map[string]Upstream `yaml:"upstreams"`
}

type Upstream struct {
	Auth      AuthConfig          `yaml:"auth"`
	Endpoints map[string]Endpoint `yaml:"endpoints"`
}

type AuthConfig struct {
	Type        string            `yaml:"type"`
	LoginURL    string            `yaml:"login_url,omitempty"`
	LoginMethod string            `yaml:"login_method,omitempty"`
	FormFields  map[string]string `yaml:"form_fields,omitempty"`
	Headers     map[string]string `yaml:"headers,omitempty"`
}

// EndpointType values.
const (
	EndpointTypeREST    = "rest"
	EndpointTypeGraphQL = "graphql"
)

// ParamType values.
const (
	ParamTypeString  = "string"
	ParamTypeInteger = "integer"
	ParamTypeDate    = "date"
)

type Endpoint struct {
	Type        string           `yaml:"type"`
	URL         string           `yaml:"url"`
	Description string           `yaml:"description"`
	Params      map[string]Param `yaml:"params"`
	Filter      string           `yaml:"filter"`
	MinInterval time.Duration    `yaml:"min_interval"`

	// GraphQL placeholders — schema-stable but unread this PR.
	Query     string            `yaml:"query,omitempty"`
	Variables map[string]string `yaml:"variables,omitempty"`

	// compiledFilter is populated by LoadConfig when Filter is non-empty.
	// Not serialised.
	compiledFilter *gojq.Code `yaml:"-"`
}

type Param struct {
	Type        string `yaml:"type"`
	Description string `yaml:"description"`
	Default     string `yaml:"default"`
	Required    bool   `yaml:"required"`
}

func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	expanded := os.Expand(string(raw), func(name string) string {
		return os.Getenv(name)
	})

	var cfg Config
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	if err := cfg.compileFilters(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	applyDefaults(&cfg)
	return &cfg, nil
}

func applyDefaults(cfg *Config) {
	if cfg.Listen == "" {
		cfg.Listen = ":8080"
	}
	if cfg.MetricsListen == "" {
		cfg.MetricsListen = ":9090"
	}
}

func (c *Config) validate() error {
	if len(c.Upstreams) == 0 {
		return fmt.Errorf("no upstreams configured")
	}
	for name, up := range c.Upstreams {
		if name == "" {
			return fmt.Errorf("upstream name must not be empty")
		}
		if strings.ContainsAny(name, "/?#") {
			return fmt.Errorf("upstream name %q contains illegal character", name)
		}
		switch up.Auth.Type {
		case "form_login":
			if up.Auth.LoginURL == "" {
				return fmt.Errorf("upstream %q: form_login requires login_url", name)
			}
			if len(up.Auth.FormFields) == 0 {
				return fmt.Errorf("upstream %q: form_login requires form_fields", name)
			}
		case "header":
			if len(up.Auth.Headers) == 0 {
				return fmt.Errorf("upstream %q: header auth requires headers", name)
			}
		default:
			return fmt.Errorf("upstream %q: unsupported auth type %q (supported: form_login, header)", name, up.Auth.Type)
		}
		if len(up.Endpoints) == 0 {
			return fmt.Errorf("upstream %q: no endpoints configured", name)
		}
		for epName, ep := range up.Endpoints {
			if epName == "" {
				return fmt.Errorf("upstream %q: endpoint name must not be empty", name)
			}
			if strings.ContainsAny(epName, "/?#") {
				return fmt.Errorf("upstream %q endpoint %q: name contains illegal character", name, epName)
			}
			if ep.URL == "" {
				return fmt.Errorf("upstream %q endpoint %q: url is required", name, epName)
			}
			if ep.MinInterval < 0 {
				return fmt.Errorf("upstream %q endpoint %q: min_interval must not be negative", name, epName)
			}
			switch ep.Type {
			case "", EndpointTypeREST, EndpointTypeGraphQL:
				// valid
			default:
				return fmt.Errorf("upstream %q endpoint %q: unsupported type %q (supported: rest, graphql)", name, epName, ep.Type)
			}
			for pName, p := range ep.Params {
				if pName == "" {
					return fmt.Errorf("upstream %q endpoint %q: param name must not be empty", name, epName)
				}
				if pName == "apikey" {
					return fmt.Errorf("upstream %q endpoint %q: param name %q is reserved", name, epName, pName)
				}
				switch p.Type {
				case "", ParamTypeString, ParamTypeInteger, ParamTypeDate:
					// valid
				default:
					return fmt.Errorf("upstream %q endpoint %q param %q: unsupported type %q (supported: string, integer, date)", name, epName, pName, p.Type)
				}
				if p.Required && p.Default != "" {
					return fmt.Errorf("upstream %q endpoint %q param %q: required and default are mutually exclusive", name, epName, pName)
				}
			}
		}
	}
	return nil
}

// compileFilters parses and compiles every endpoint's jq filter. Invalid
// expressions cause config-load to fail rather than surfacing per-request.
func (c *Config) compileFilters() error {
	for upName, up := range c.Upstreams {
		for epName, ep := range up.Endpoints {
			if ep.Filter == "" {
				continue
			}
			parsed, err := gojq.Parse(ep.Filter)
			if err != nil {
				return fmt.Errorf("upstream %q endpoint %q: parse filter: %w", upName, epName, err)
			}
			compiled, err := gojq.Compile(parsed)
			if err != nil {
				return fmt.Errorf("upstream %q endpoint %q: compile filter: %w", upName, epName, err)
			}
			ep.compiledFilter = compiled
			up.Endpoints[epName] = ep
		}
		c.Upstreams[upName] = up
	}
	return nil
}

// endpointType returns the effective type for an endpoint, defaulting to rest
// when empty.
func (e Endpoint) endpointType() string {
	if e.Type == "" {
		return EndpointTypeREST
	}
	return e.Type
}

// paramType returns the effective type for a param, defaulting to string.
func (p Param) paramType() string {
	if p.Type == "" {
		return ParamTypeString
	}
	return p.Type
}
