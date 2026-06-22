package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server    ServerConfig     `yaml:"server"`
	Gateway   GatewayConfig    `yaml:"gateway"`
	Upstreams []UpstreamConfig `yaml:"upstreams"`
	Routing   RoutingConfig    `yaml:"routing"`
}

type ServerConfig struct {
	Port int `yaml:"port"`
}

type GatewayConfig struct {
	Keys []string `yaml:"keys"`
}

type UpstreamConfig struct {
	Name    string            `yaml:"name"`
	URL     string            `yaml:"url"`
	Key     string            `yaml:"key"`
	Models  []string          `yaml:"models"`
	Aliases map[string]string `yaml:"aliases"`
}

type RoutingConfig struct {
	Timeout time.Duration `yaml:"timeout"`
}

func Load(path string) (*Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open config file at %q: %w", path, err)
	}
	defer file.Close()

	var cfg Config
	decoder := yaml.NewDecoder(file)
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		if isLegacyUpstreamErr(err) {
			return nil, fmt.Errorf("failed to decode config yaml at %q: %w\n\nThe single 'upstream' field was removed in favor of 'upstreams[]'. Migrate your config:\n  upstream:\n    url: \"https://api.deepseek.com/v1\"\n    key: \"sk-...\"\n  becomes:\n  upstreams:\n    - name: deepseek\n      url: \"https://api.deepseek.com/v1\"\n      key: \"sk-...\"\n      models:\n        - deepseek-chat\n\nSee config.example.yaml for a working example.", path, err)
		}
		return nil, fmt.Errorf("failed to decode config yaml at %q: %w", path, err)
	}

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	return &cfg, nil
}

func (c *Config) validate() error {
	if len(c.Upstreams) == 0 {
		return fmt.Errorf("at least one upstream must be configured")
	}

	seenModels := make(map[string]string)
	for i, u := range c.Upstreams {
		if u.Name == "" {
			return fmt.Errorf("upstream[%d]: name is required", i)
		}
		if u.URL == "" {
			return fmt.Errorf("upstream %q: url is required", u.Name)
		}
		if len(u.Models) == 0 {
			return fmt.Errorf("upstream %q: at least one model must be listed", u.Name)
		}
		for _, m := range u.Models {
			if prev, exists := seenModels[m]; exists {
				return fmt.Errorf("model %q is claimed by both upstream %q and %q", m, prev, u.Name)
			}
			seenModels[m] = u.Name
		}
		for alias, target := range u.Aliases {
			if _, exists := seenModels[alias]; exists {
				return fmt.Errorf("alias %q in upstream %q collides with an already-declared model", alias, u.Name)
			}
			if _, ok := seenModels[target]; !ok {
				return fmt.Errorf("alias %q -> %q in upstream %q points to unknown model", alias, target, u.Name)
			}
			seenModels[alias] = u.Name
		}
	}

	if c.Routing.Timeout <= 0 {
		c.Routing.Timeout = 120 * time.Second
	}

	return nil
}

// isLegacyUpstreamErr detects the specific KnownFields error produced when a
// config file still uses the pre-Phase-1 single 'upstream' field, so Load can
// return a helpful migration message instead of a bare unmarshal error.
func isLegacyUpstreamErr(err error) bool {
	return strings.Contains(err.Error(), "field upstream not found")
}
