package ratelimit_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/cvcraft252/llm-gateway/internal/middleware"
	"github.com/cvcraft252/llm-gateway/internal/ratelimit"
)

func TestLimiterRegistry_Allow(t *testing.T) {
	t.Parallel()

	// 3 RPM = burst of 3, refill every 20s
	r := ratelimit.NewRegistry(3)

	// First 3 requests should be allowed (burst)
	for i := 0; i < 3; i++ {
		allowed, _ := r.Allow("gw-testkey1")
		if !allowed {
			t.Errorf("request %d should be allowed within burst, got denied", i+1)
		}
	}

	// 4th request should be denied
	allowed, retryAfter := r.Allow("gw-testkey1")
	if allowed {
		t.Error("4th request should be rate-limited")
	}
	if retryAfter <= 0 {
		t.Error("retryAfter should be positive when rate-limited")
	}
}

func TestLimiterRegistry_DifferentKeysIndependent(t *testing.T) {
	t.Parallel()

	r := ratelimit.NewRegistry(2)

	// Key A uses both tokens
	r.Allow("gw-keyA")
	r.Allow("gw-keyA")

	// Key A is now limited
	allowed, _ := r.Allow("gw-keyA")
	if allowed {
		t.Error("keyA should be limited after 2 requests")
	}

	// Key B should still have its full burst
	allowed, _ = r.Allow("gw-keyB")
	if !allowed {
		t.Error("keyB should be independent of keyA's limit")
	}
}

func TestLimiterRegistry_Disabled(t *testing.T) {
	t.Parallel()

	r := ratelimit.NewRegistry(0) // disabled

	for i := 0; i < 100; i++ {
		allowed, _ := r.Allow("gw-anykey")
		if !allowed {
			t.Errorf("request %d should be allowed when rate limiting is disabled", i)
		}
	}
}

func TestLimiterRegistry_Concurrent(t *testing.T) {
	t.Parallel()

	// 100 RPM, 50 goroutines hitting it concurrently
	r := ratelimit.NewRegistry(100)
	var wg sync.WaitGroup
	allowedCount := 0
	var countMu sync.Mutex

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			allowed, _ := r.Allow("gw-concurrent")
			if allowed {
				countMu.Lock()
				allowedCount++
				countMu.Unlock()
			}
		}()
	}
	wg.Wait()

	// Exactly 100 should be allowed (burst capacity), rest denied
	if allowedCount != 50 {
		t.Errorf("allowed %d, want 50 (all within burst)", allowedCount)
	}
}

func TestLimiterRegistry_Middleware_AllowsWithinBurst(t *testing.T) {
	t.Parallel()

	r := ratelimit.NewRegistry(5)
	nextCalled := 0
	next := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		nextCalled++
		w.WriteHeader(http.StatusOK)
	})
	handler := r.Middleware(next)

	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		req = req.WithContext(setKeyID(req.Context(), "gw-test1234"))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("request %d: status = %d, want 200", i+1, rec.Code)
		}
	}
	if nextCalled != 5 {
		t.Errorf("next called %d times, want 5", nextCalled)
	}
}

func TestLimiterRegistry_Middleware_DeniesOverBurst(t *testing.T) {
	t.Parallel()

	r := ratelimit.NewRegistry(2)
	next := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := r.Middleware(next)

	// First 2 pass
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		req = req.WithContext(setKeyID(req.Context(), "gw-limitme1"))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
	}

	// 3rd should be 429
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req = req.WithContext(setKeyID(req.Context(), "gw-limitme1"))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("3rd request: status = %d, want 429", rec.Code)
	}
	if ra := rec.Header().Get("Retry-After"); ra == "" {
		t.Error("Retry-After header should be set on 429")
	}
}

func TestLimiterRegistry_Middleware_NoKeyIDPasses(t *testing.T) {
	t.Parallel()

	r := ratelimit.NewRegistry(1)
	next := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := r.Middleware(next)

	// No key_id in context (no auth middleware ran) → should pass
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (no key_id = no rate limit)", rec.Code)
	}
}

func TestLimiterRegistry_Middleware_Disabled(t *testing.T) {
	t.Parallel()

	r := ratelimit.NewRegistry(0) // disabled
	next := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := r.Middleware(next)

	for i := 0; i < 10; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		req = req.WithContext(setKeyID(req.Context(), "gw-anykey"))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("request %d: status = %d, want 200 (disabled)", i+1, rec.Code)
		}
	}
}

// setKeyID is a test helper that sets the key_id in context like the auth
// middleware would.
func setKeyID(ctx context.Context, keyID string) context.Context {
	return context.WithValue(ctx, middleware.CtxKeyKeyID, keyID)
}
