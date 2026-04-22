package main

import (
	"fmt"
	"os"
	"strings"
	"time"

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

type Endpoint struct {
	URL           string        `yaml:"url"`
	AllowedParams []string      `yaml:"allowed_params"`
	MinInterval   time.Duration `yaml:"min_interval"`
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
		}
	}
	return nil
}
