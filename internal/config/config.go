package config

import (
	"fmt"
	"os"
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
	Name     string            `yaml:"name"`
	URL      string            `yaml:"url"`
	Key      string            `yaml:"key"`
	Models   []string          `yaml:"models"`
	Aliases  map[string]string `yaml:"aliases"`
	Fallback string            `yaml:"fallback"`
}

type RoutingConfig struct {
	Timeout           time.Duration `yaml:"timeout"`
	MaxRetries        int           `yaml:"max_retries"`
	RetryBackoff      time.Duration `yaml:"retry_backoff"`
	HealthMaxFailures int           `yaml:"health_max_failures"`
	HealthCooldown    time.Duration `yaml:"health_cooldown"`
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
	upstreamNames := make(map[string]bool)
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
		upstreamNames[u.Name] = true
	}

	for _, u := range c.Upstreams {
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

		if u.Fallback != "" {
			if u.Fallback == u.Name {
				return fmt.Errorf("upstream %q cannot fall back to itself", u.Name)
			}
			if !upstreamNames[u.Fallback] {
				return fmt.Errorf("fallback %q for upstream %q not found", u.Fallback, u.Name)
			}
		}
	}

	if c.Routing.Timeout <= 0 {
		c.Routing.Timeout = 120 * time.Second
	}

	if c.Routing.MaxRetries < 0 {
		c.Routing.MaxRetries = 0
	}

	if c.Routing.RetryBackoff <= 0 {
		c.Routing.RetryBackoff = 500 * time.Millisecond
	}

	if c.Routing.HealthMaxFailures <= 0 {
		c.Routing.HealthMaxFailures = 3
	}

	if c.Routing.HealthCooldown <= 0 {
		c.Routing.HealthCooldown = 30 * time.Second
	}

	return nil
}
