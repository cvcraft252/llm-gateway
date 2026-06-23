package router_test

import (
	"errors"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/cvcraft252/llm-gateway/internal/config"
	"github.com/cvcraft252/llm-gateway/internal/router"
)

func validUpstream(name, baseURL string, models []string, aliases map[string]string) config.UpstreamConfig {
	return config.UpstreamConfig{
		Name:    name,
		URL:     baseURL,
		Key:     "sk-test",
		Models:  models,
		Aliases: aliases,
	}
}

func TestNew_AndPick(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		Upstreams: []config.UpstreamConfig{
			validUpstream("deepseek", "https://api.deepseek.com/v1",
				[]string{"deepseek-chat", "deepseek-reasoner"}, nil),
			validUpstream("openai", "https://api.openai.com/v1",
				[]string{"gpt-4o", "gpt-4o-mini"},
				map[string]string{"gpt-4": "gpt-4o", "gpt-4-turbo": "gpt-4o"}),
		},
		Routing: config.RoutingConfig{Timeout: 30 * time.Second},
	}

	rtr, err := router.New(cfg)
	if err != nil {
		t.Fatalf("New: unexpected error: %v", err)
	}

	if rtr.RequestTimeout() != 30*time.Second {
		t.Errorf("RequestTimeout = %v, want 30s", rtr.RequestTimeout())
	}

	tests := []struct {
		name            string
		model           string
		wantUpstream    string
		wantTargetModel string
		wantErr         bool
	}{
		{"exact match deepseek-chat", "deepseek-chat", "deepseek", "deepseek-chat", false},
		{"exact match deepseek-reasoner", "deepseek-reasoner", "deepseek", "deepseek-reasoner", false},
		{"exact match gpt-4o", "gpt-4o", "openai", "gpt-4o", false},
		{"alias gpt-4 resolves to gpt-4o", "gpt-4", "openai", "gpt-4o", false},
		{"alias gpt-4-turbo resolves to gpt-4o", "gpt-4-turbo", "openai", "gpt-4o", false},
		{"unknown model", "claude-3", "", "", true},
		{"empty model", "", "", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			up, target, err := rtr.Pick(tt.model)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Pick: expected error, got nil")
				}
				if !errors.Is(err, router.ErrModelNotFound) {
					t.Errorf("Pick: error = %v, want ErrModelNotFound", err)
				}
				if up != nil {
					t.Errorf("Pick: expected nil upstream on error, got %+v", up)
				}
				return
			}
			if err != nil {
				t.Fatalf("Pick: unexpected error: %v", err)
			}
			if up.Name != tt.wantUpstream {
				t.Errorf("upstream name = %q, want %q", up.Name, tt.wantUpstream)
			}
			if target != tt.wantTargetModel {
				t.Errorf("target model = %q, want %q", target, tt.wantTargetModel)
			}
		})
	}
}

func TestNew_RejectsInvalidConfigs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cfg     *config.Config
		wantErr string
	}{
		{
			name:    "nil config",
			cfg:     nil,
			wantErr: "config is nil",
		},
		{
			name:    "no upstreams",
			cfg:     &config.Config{Upstreams: nil},
			wantErr: "no upstreams configured",
		},
		{
			name: "invalid url scheme",
			cfg: &config.Config{
				Upstreams: []config.UpstreamConfig{
					validUpstream("bad", "://no-scheme", []string{"m1"}, nil),
				},
			},
			wantErr: "invalid url",
		},
		{
			name: "missing host",
			cfg: &config.Config{
				Upstreams: []config.UpstreamConfig{
					validUpstream("bad", "https://", []string{"m1"}, nil),
				},
			},
			wantErr: "must include scheme and host",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := router.New(tt.cfg)
			if err == nil {
				t.Fatalf("New: expected error containing %q, got nil", tt.wantErr)
			}
			if !contains(err.Error(), tt.wantErr) {
				t.Errorf("New: error = %q, want substring %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestNew_UpstreamURLParsedCorrectly(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		Upstreams: []config.UpstreamConfig{
			validUpstream("local", "http://localhost:11434/v1", []string{"llama3"}, nil),
		},
	}

	rtr, err := router.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	up, _, err := rtr.Pick("llama3")
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}

	if up.URL.Scheme != "http" {
		t.Errorf("URL.Scheme = %q, want http", up.URL.Scheme)
	}
	if up.URL.Host != "localhost:11434" {
		t.Errorf("URL.Host = %q, want localhost:11434", up.URL.Host)
	}
	if up.URL.Path != "/v1" {
		t.Errorf("URL.Path = %q, want /v1", up.URL.Path)
	}
	if _, err := url.Parse(up.URL.String()); err != nil {
		t.Errorf("URL.String() = %q, not re-parseable: %v", up.URL.String(), err)
	}
}

func TestPick_Concurrent(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		Upstreams: []config.UpstreamConfig{
			validUpstream("a", "https://a/v1", []string{"m-a-1", "m-a-2"}, nil),
			validUpstream("b", "https://b/v1", []string{"m-b-1"}, map[string]string{"alias-b": "m-b-1"}),
		},
	}

	rtr, err := router.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	models := []string{"m-a-1", "m-a-2", "m-b-1", "alias-b", "unknown"}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			m := models[i%len(models)]
			_, _, err := rtr.Pick(m)
			if m == "unknown" {
				if !errors.Is(err, router.ErrModelNotFound) {
					t.Errorf("Pick(%q): expected ErrModelNotFound, got %v", m, err)
				}
				return
			}
			if err != nil {
				t.Errorf("Pick(%q): unexpected error: %v", m, err)
			}
		}(i)
	}
	wg.Wait()
}

func TestPick_AliasOnlyNotInModels(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		Upstreams: []config.UpstreamConfig{
			validUpstream("a", "https://a/v1",
				[]string{"real-model"},
				map[string]string{"alias-only": "real-model"}),
		},
	}

	rtr, err := router.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	up, target, err := rtr.Pick("alias-only")
	if err != nil {
		t.Fatalf("Pick(alias-only): %v", err)
	}
	if up.Name != "a" {
		t.Errorf("upstream = %q, want a", up.Name)
	}
	if target != "real-model" {
		t.Errorf("target = %q, want real-model", target)
	}

	up2, target2, err := rtr.Pick("real-model")
	if err != nil {
		t.Fatalf("Pick(real-model): %v", err)
	}
	if up2.Name != "a" {
		t.Errorf("upstream = %q, want a", up2.Name)
	}
	if target2 != "real-model" {
		t.Errorf("target = %q, want real-model", target2)
	}
}

func TestNew_CircularFallbackDetected(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		upstreams []config.UpstreamConfig
		wantErr   string
	}{
		{
			name: "two-node cycle A -> B -> A",
			upstreams: []config.UpstreamConfig{
				{Name: "a", URL: "https://a/v1", Key: "k", Models: []string{"m-a"}, Fallback: "b"},
				{Name: "b", URL: "https://b/v1", Key: "k", Models: []string{"m-b"}, Fallback: "a"},
			},
			wantErr: "circular fallback detected",
		},
		{
			name: "three-node cycle A -> B -> C -> A",
			upstreams: []config.UpstreamConfig{
				{Name: "a", URL: "https://a/v1", Key: "k", Models: []string{"m-a"}, Fallback: "b"},
				{Name: "b", URL: "https://b/v1", Key: "k", Models: []string{"m-b"}, Fallback: "c"},
				{Name: "c", URL: "https://c/v1", Key: "k", Models: []string{"m-c"}, Fallback: "a"},
			},
			wantErr: "circular fallback detected",
		},
		{
			name: "valid chain A -> B -> C (no cycle)",
			upstreams: []config.UpstreamConfig{
				{Name: "a", URL: "https://a/v1", Key: "k", Models: []string{"m-a"}, Fallback: "b"},
				{Name: "b", URL: "https://b/v1", Key: "k", Models: []string{"m-b"}, Fallback: "c"},
				{Name: "c", URL: "https://c/v1", Key: "k", Models: []string{"m-c"}},
			},
			wantErr: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := &config.Config{Upstreams: tt.upstreams}
			_, err := router.New(cfg)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("New: expected error containing %q, got nil", tt.wantErr)
				}
				if !contains(err.Error(), tt.wantErr) {
					t.Errorf("New: error = %q, want substring %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("New: unexpected error: %v", err)
			}
		})
	}
}

func TestNew_FallbackResolvesCorrectly(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		Upstreams: []config.UpstreamConfig{
			{Name: "primary", URL: "https://p/v1", Key: "k", Models: []string{"m1"}, Fallback: "secondary"},
			{Name: "secondary", URL: "https://s/v1", Key: "k", Models: []string{"m2"}},
		},
	}

	rtr, err := router.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	up, _, err := rtr.Pick("m1")
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if up.Fallback == nil {
		t.Fatalf("primary upstream should have a fallback")
	}
	if up.Fallback.Name != "secondary" {
		t.Errorf("fallback name = %q, want %q", up.Fallback.Name, "secondary")
	}
	if up.Fallback.Fallback != nil {
		t.Errorf("secondary upstream should not have a fallback")
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
