package ratelimit

import (
	"net/http"
	"sync"
	"time"

	"github.com/cvcraft252/llm-gateway/internal/middleware"
	"github.com/cvcraft252/llm-gateway/internal/respond"
)

// LimiterRegistry maintains a per-key-id rate.Limiter registry.
// Each key gets its own token bucket on first use (lazy init).
// The registry is safe for concurrent access via RWMutex.
//
// Token bucket parameters:
//
//	burst = defaultRPM  (allows a burst of defaultRPM requests at once)
//	rate  = defaultRPM/60 per second (steady-state replenishment)
//
// This means a key with 60 RPM can send 60 requests instantly, then
// 1 request/second sustained. Burst = RPM (not a fixed small number)
// because LLM workloads are bursty (batch processing, concurrent tools).
type LimiterRegistry struct {
	mu         sync.RWMutex
	limiters   map[string]*limiterEntry
	defaultRPM int
}

type limiterEntry struct {
	tokens     chan time.Time
	burst      int
	refillRate time.Duration // interval between token refills
	lastRefill time.Time
	mu         sync.Mutex
}

// NewRegistry creates a LimiterRegistry with the given default RPM.
// If defaultRPM <= 0, no rate limiting is applied (all requests pass).
func NewRegistry(defaultRPM int) *LimiterRegistry {
	if defaultRPM <= 0 {
		return &LimiterRegistry{limiters: nil, defaultRPM: 0}
	}
	return &LimiterRegistry{
		limiters:   make(map[string]*limiterEntry),
		defaultRPM: defaultRPM,
	}
}

// Allow checks whether the key identified by keyID is allowed to make
// a request. Returns true if allowed, false if rate-limited.
// On false, returns the duration to wait before the next request can
// be made (for Retry-After header).
func (r *LimiterRegistry) Allow(keyID string) (allowed bool, retryAfter time.Duration) {
	if r.defaultRPM <= 0 {
		return true, 0
	}

	// YAML keys (keyID == "yaml") share a single bucket
	bucketKey := keyID
	if bucketKey == "yaml" {
		bucketKey = "__yaml__"
	}

	entry := r.getOrCreate(bucketKey)
	return entry.allow()
}

func (r *LimiterRegistry) getOrCreate(keyID string) *limiterEntry {
	r.mu.RLock()
	entry, ok := r.limiters[keyID]
	r.mu.RUnlock()
	if ok {
		return entry
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	// Double-check after acquiring write lock
	if entry, ok := r.limiters[keyID]; ok {
		return entry
	}

	burst := r.defaultRPM
	refillInterval := time.Minute / time.Duration(r.defaultRPM)

	entry = &limiterEntry{
		tokens:     make(chan time.Time, burst),
		burst:      burst,
		refillRate: refillInterval,
		lastRefill: time.Now(),
	}
	// Pre-fill the bucket so the first burst is allowed immediately
	for i := 0; i < burst; i++ {
		entry.tokens <- time.Now()
	}
	r.limiters[keyID] = entry
	return entry
}

// allow atomically consumes a token or computes the wait time.
func (e *limiterEntry) allow() (bool, time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()

	// Refill tokens based on elapsed time
	now := time.Now()
	elapsed := now.Sub(e.lastRefill)
	tokensToAdd := int(elapsed / e.refillRate)
	if tokensToAdd > 0 {
		for i := 0; i < tokensToAdd && len(e.tokens) < e.burst; i++ {
			e.tokens <- now
		}
		e.lastRefill = e.lastRefill.Add(time.Duration(tokensToAdd) * e.refillRate)
	}

	// Try to consume a token
	select {
	case <-e.tokens:
		return true, 0
	default:
		// Bucket empty — calculate when the next token will be available
		wait := e.refillRate - (now.Sub(e.lastRefill))
		if wait <= 0 {
			wait = e.refillRate
		}
		return false, wait
	}
}

// Middleware returns an HTTP middleware that enforces per-key rate limiting.
// It reads the key_id from the request context (set by auth middleware).
// If rate limited, responds with 429 and Retry-After header.
// If no key_id in context (no auth), the request is allowed (rate limiting
// only applies to authenticated keys).
func (r *LimiterRegistry) Middleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if r.defaultRPM <= 0 {
			next.ServeHTTP(w, req)
			return
		}

		keyID, _ := req.Context().Value(middleware.CtxKeyKeyID).(string)
		if keyID == "" {
			next.ServeHTTP(w, req)
			return
		}

		allowed, retryAfter := r.Allow(keyID)
		if !allowed {
			w.Header().Set("Retry-After", formatRetryAfter(retryAfter))
			respond.WriteJSONError(w, http.StatusTooManyRequests, "Rate limit exceeded")
			return
		}

		next.ServeHTTP(w, req)
	}
}

// formatRetryAfter converts a duration to the integer seconds expected
// by the HTTP Retry-After header, with a minimum of 1 second.
func formatRetryAfter(d time.Duration) string {
	seconds := int(d.Seconds())
	if seconds < 1 {
		seconds = 1
	}
	return formatInt(seconds)
}

func formatInt(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}
