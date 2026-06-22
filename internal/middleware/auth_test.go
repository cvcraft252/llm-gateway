package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cvcraft252/llm-gateway/internal/config"
	"github.com/cvcraft252/llm-gateway/internal/middleware"
)

func TestAuth(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		authHeader     string
		keys           []string
		wantStatus     int
		wantBodySubstr string
		wantNextCalled bool
	}{
		{
			name:           "missing authorization header",
			authHeader:     "",
			keys:           []string{"gw-key-valid"},
			wantStatus:     http.StatusUnauthorized,
			wantBodySubstr: "Missing Authorization header",
			wantNextCalled: false,
		},
		{
			name:           "invalid format - no bearer prefix",
			authHeader:     "Token gw-key-valid",
			keys:           []string{"gw-key-valid"},
			wantStatus:     http.StatusUnauthorized,
			wantBodySubstr: "Invalid Authorization header format",
			wantNextCalled: false,
		},
		{
			name:           "invalid format - only one part",
			authHeader:     "Bearer",
			keys:           []string{"gw-key-valid"},
			wantStatus:     http.StatusUnauthorized,
			wantBodySubstr: "Invalid Authorization header format",
			wantNextCalled: false,
		},
		{
			name:           "invalid format - three parts",
			authHeader:     "Bearer gw-key-valid extra",
			keys:           []string{"gw-key-valid"},
			wantStatus:     http.StatusUnauthorized,
			wantBodySubstr: "Invalid Authorization header format",
			wantNextCalled: false,
		},
		{
			name:           "invalid token",
			authHeader:     "Bearer gw-key-wrong",
			keys:           []string{"gw-key-valid"},
			wantStatus:     http.StatusUnauthorized,
			wantBodySubstr: "Unauthorized key",
			wantNextCalled: false,
		},
		{
			name:           "valid first key",
			authHeader:     "Bearer gw-key-valid",
			keys:           []string{"gw-key-valid", "gw-key-alt"},
			wantStatus:     http.StatusOK,
			wantBodySubstr: `{"ok": true}`,
			wantNextCalled: true,
		},
		{
			name:           "valid second key",
			authHeader:     "Bearer gw-key-alt",
			keys:           []string{"gw-key-valid", "gw-key-alt"},
			wantStatus:     http.StatusOK,
			wantBodySubstr: `{"ok": true}`,
			wantNextCalled: true,
		},
		{
			name:           "case-insensitive bearer scheme",
			authHeader:     "bearer gw-key-valid",
			keys:           []string{"gw-key-valid"},
			wantStatus:     http.StatusOK,
			wantBodySubstr: `{"ok": true}`,
			wantNextCalled: true,
		},
		{
			name:           "case-insensitive BEARER scheme",
			authHeader:     "BEARER gw-key-valid",
			keys:           []string{"gw-key-valid"},
			wantStatus:     http.StatusOK,
			wantBodySubstr: `{"ok": true}`,
			wantNextCalled: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			nextCalled := false
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				nextCalled = true
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"ok": true}`))
			})

			cfg := &config.Config{
				Gateway: config.GatewayConfig{Keys: tt.keys},
			}
			handler := middleware.Auth(cfg, next)

			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			body := rec.Body.String()
			if tt.wantBodySubstr != "" && !contains(body, tt.wantBodySubstr) {
				t.Errorf("body = %q, want substring %q", body, tt.wantBodySubstr)
			}
			if nextCalled != tt.wantNextCalled {
				t.Errorf("next called = %v, want %v", nextCalled, tt.wantNextCalled)
			}
		})
	}
}

func TestAuth_ErrorResponseContentType(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		Gateway: config.GatewayConfig{Keys: []string{"gw-key-valid"}},
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("next handler should not be called on auth failure")
	})
	handler := middleware.Auth(cfg, next)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want %q", ct, "application/json")
	}
}

func TestAuth_EmptyKeyList(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{Gateway: config.GatewayConfig{Keys: nil}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("next handler should not be called when no keys configured")
	})
	handler := middleware.Auth(cfg, next)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer anything")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d (no keys configured should reject all)", rec.Code, http.StatusUnauthorized)
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
