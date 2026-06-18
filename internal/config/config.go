package config

import (
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server   ServerConfig   `yaml:"server"`
	Gateway  GatewayConfig  `yaml:"gateway"`
	Upstream UpstreamConfig `yaml:"upstream"`
}

type ServerConfig struct {
	Port int `yaml:"port"`
}

type GatewayConfig struct {
	Keys []string `yaml:"keys"`
}

type UpstreamConfig struct {
	URL string `yaml:"url"`
	Key string `yaml:"key"`
}

func Load(path string) (*Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var cfg Config
	decoder := yaml.NewDecoder(file)
	if err := decoder.Decode(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}
