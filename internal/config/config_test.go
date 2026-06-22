package config_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cvcraft252/llm-gateway/internal/config"
)

func TestLoad(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		content   string
		wantErr   bool
		errSubstr string
		check     func(t *testing.T, cfg *config.Config)
	}{
		{
			name: "valid multi-upstream config with aliases",
			content: `server:
  port: 8080
gateway:
  keys:
    - gw-key-1
upstreams:
  - name: deepseek
    url: "https://api.deepseek.com/v1"
    key: "sk-deepseek"
    models:
      - deepseek-chat
      - deepseek-reasoner
  - name: openai
    url: "https://api.openai.com/v1"
    key: "sk-openai"
    models:
      - gpt-4o
      - gpt-4o-mini
    aliases:
      gpt-4: gpt-4o
      gpt-4-turbo: gpt-4o
routing:
  timeout: 60s
`,
			wantErr: false,
			check: func(t *testing.T, cfg *config.Config) {
				t.Helper()
				if cfg.Server.Port != 8080 {
					t.Errorf("Server.Port = %d, want 8080", cfg.Server.Port)
				}
				if len(cfg.Gateway.Keys) != 1 {
					t.Errorf("len(Gateway.Keys) = %d, want 1", len(cfg.Gateway.Keys))
				}
				if len(cfg.Upstreams) != 2 {
					t.Fatalf("len(Upstreams) = %d, want 2", len(cfg.Upstreams))
				}
				if cfg.Upstreams[0].Name != "deepseek" {
					t.Errorf("Upstreams[0].Name = %q, want %q", cfg.Upstreams[0].Name, "deepseek")
				}
				if cfg.Upstreams[1].URL != "https://api.openai.com/v1" {
					t.Errorf("Upstreams[1].URL = %q, want %q", cfg.Upstreams[1].URL, "https://api.openai.com/v1")
				}
				if cfg.Upstreams[1].Key != "sk-openai" {
					t.Errorf("Upstreams[1].Key = %q, want %q", cfg.Upstreams[1].Key, "sk-openai")
				}
				if cfg.Upstreams[1].Aliases["gpt-4"] != "gpt-4o" {
					t.Errorf("alias gpt-4 = %q, want %q", cfg.Upstreams[1].Aliases["gpt-4"], "gpt-4o")
				}
				if cfg.Routing.Timeout != 60*time.Second {
					t.Errorf("Routing.Timeout = %v, want 60s", cfg.Routing.Timeout)
				}
			},
		},
		{
			name: "default timeout when unspecified",
			content: `server:
  port: 8080
gateway:
  keys:
    - gw-key-1
upstreams:
  - name: local
    url: "http://localhost:11434/v1"
    key: ""
    models:
      - llama3
`,
			wantErr: false,
			check: func(t *testing.T, cfg *config.Config) {
				t.Helper()
				if cfg.Routing.Timeout != 120*time.Second {
					t.Errorf("default Routing.Timeout = %v, want 120s", cfg.Routing.Timeout)
				}
			},
		},
		{
			name: "empty yaml yields zero upstreams and validation error",
			content: `server: {}
gateway: {}
upstreams: []
`,
			wantErr:   true,
			errSubstr: "at least one upstream must be configured",
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
		{
			name: "legacy single upstream schema is rejected",
			content: `server:
  port: 8080
gateway:
  keys:
    - gw-key-1
upstream:
  url: "https://api.deepseek.com/v1"
  key: "sk-real"
`,
			wantErr:   true,
			errSubstr: "failed to decode config yaml",
		},
		{
			name: "upstream missing name",
			content: `server:
  port: 8080
gateway:
  keys:
    - gw-key-1
upstreams:
  - url: "https://api.deepseek.com/v1"
    key: "sk"
    models:
      - deepseek-chat
`,
			wantErr:   true,
			errSubstr: "name is required",
		},
		{
			name: "upstream missing url",
			content: `server:
  port: 8080
upstreams:
  - name: deepseek
    key: "sk"
    models:
      - deepseek-chat
`,
			wantErr:   true,
			errSubstr: "url is required",
		},
		{
			name: "upstream with no models",
			content: `server:
  port: 8080
upstreams:
  - name: deepseek
    url: "https://api.deepseek.com/v1"
    key: "sk"
`,
			wantErr:   true,
			errSubstr: "at least one model",
		},
		{
			name: "duplicate model across upstreams",
			content: `server:
  port: 8080
upstreams:
  - name: a
    url: "https://a/v1"
    key: "sk"
    models:
      - shared-model
  - name: b
    url: "https://b/v1"
    key: "sk"
    models:
      - shared-model
`,
			wantErr:   true,
			errSubstr: "is claimed by both",
		},
		{
			name: "alias collides with declared model",
			content: `server:
  port: 8080
upstreams:
  - name: a
    url: "https://a/v1"
    key: "sk"
    models:
      - gpt-4o
    aliases:
      gpt-4o: gpt-4o
`,
			wantErr:   true,
			errSubstr: "collides with an already-declared model",
		},
		{
			name: "alias points to unknown model",
			content: `server:
  port: 8080
upstreams:
  - name: a
    url: "https://a/v1"
    key: "sk"
    models:
      - gpt-4o
    aliases:
      gpt-4: nonexistent
`,
			wantErr:   true,
			errSubstr: "points to unknown model",
		},
		{
			name: "unknown top-level field is rejected by strict decode",
			content: `server:
  port: 8080
upstreams:
  - name: a
    url: "https://a/v1"
    key: "sk"
    models:
      - m1
unknown_field: 42
`,
			wantErr:   true,
			errSubstr: "failed to decode config yaml",
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
			if tt.check != nil {
				tt.check(t, cfg)
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
