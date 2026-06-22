package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cvcraft252/llm-gateway/internal/config"
)

func TestLoad(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		content   string
		wantErr   bool
		wantPort  int
		wantKeys  int
		wantURL   string
		wantKey   string
		errSubstr string
	}{
		{
			name: "valid full config",
			content: `server:
  port: 8080
gateway:
  keys:
    - gw-key-123456
    - gw-key-789
upstream:
  url: "https://api.deepseek.com/v1"
  key: "sk-real-key"
`,
			wantErr:  false,
			wantPort: 8080,
			wantKeys: 2,
			wantURL:  "https://api.deepseek.com/v1",
			wantKey:  "sk-real-key",
		},
		{
			name:     "empty yaml yields zero values",
			content:  "server: {}\ngateway: {}\nupstream: {}\n",
			wantErr:  false,
			wantPort: 0,
			wantKeys: 0,
			wantURL:  "",
			wantKey:  "",
		},
		{
			name:      "invalid yaml",
			content:   `server: [unterminated`,
			wantErr:   true,
			errSubstr: "failed to decode config yaml",
		},
		{
			name:      "missing file",
			content:   "",
			wantErr:   true,
			errSubstr: "failed to open config file",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "config.yaml")

			if tt.name != "missing file" {
				if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
					t.Fatalf("setup: write config: %v", err)
				}
			} else {
				path = filepath.Join(t.TempDir(), "does-not-exist.yaml")
			}

			cfg, err := config.Load(path)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Load: expected error, got nil")
				}
				if tt.errSubstr != "" && !contains(err.Error(), tt.errSubstr) {
					t.Fatalf("Load: error %q does not contain %q", err.Error(), tt.errSubstr)
				}
				if cfg != nil {
					t.Fatalf("Load: expected nil config on error, got %+v", cfg)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: unexpected error: %v", err)
			}
			if cfg == nil {
				t.Fatalf("Load: expected non-nil config")
			}
			if cfg.Server.Port != tt.wantPort {
				t.Errorf("Server.Port = %d, want %d", cfg.Server.Port, tt.wantPort)
			}
			if len(cfg.Gateway.Keys) != tt.wantKeys {
				t.Errorf("len(Gateway.Keys) = %d, want %d", len(cfg.Gateway.Keys), tt.wantKeys)
			}
			if cfg.Upstream.URL != tt.wantURL {
				t.Errorf("Upstream.URL = %q, want %q", cfg.Upstream.URL, tt.wantURL)
			}
			if cfg.Upstream.Key != tt.wantKey {
				t.Errorf("Upstream.Key = %q, want %q", cfg.Upstream.Key, tt.wantKey)
			}
		})
	}
}

func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
