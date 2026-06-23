package health

import (
	"sync"
	"time"
)

// Tracker monitors the health of upstreams by counting consecutive failures.
// It is thread-safe and safe for concurrent use.
type Tracker struct {
	mu               sync.RWMutex
	consecutiveFail  map[string]int
	degradedUntil    map[string]time.Time
	maxFailures      int
	coolDownDuration time.Duration
}

// NewTracker constructs a new health Tracker with the given failure threshold and cooldown.
func NewTracker(maxFailures int, coolDownDuration time.Duration) *Tracker {
	if maxFailures <= 0 {
		maxFailures = 3
	}
	if coolDownDuration <= 0 {
		coolDownDuration = 30 * time.Second
	}
	return &Tracker{
		consecutiveFail:  make(map[string]int),
		degradedUntil:    make(map[string]time.Time),
		maxFailures:      maxFailures,
		coolDownDuration: coolDownDuration,
	}
}

// RecordSuccess resets the consecutive failures and removes the degradation status.
func (t *Tracker) RecordSuccess(upstream string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.consecutiveFail[upstream] = 0
	delete(t.degradedUntil, upstream)
}

// RecordFailure increments the consecutive failure count. If it reaches the threshold,
// the upstream is marked as degraded for the cooldown duration.
func (t *Tracker) RecordFailure(upstream string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.consecutiveFail[upstream]++
	if t.consecutiveFail[upstream] >= t.maxFailures {
		t.degradedUntil[upstream] = time.Now().Add(t.coolDownDuration)
	}
}

// IsDegraded returns true if the upstream is currently marked as degraded and the cooldown
// period has not yet elapsed.
func (t *Tracker) IsDegraded(upstream string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	until, exists := t.degradedUntil[upstream]
	if !exists {
		return false
	}
	if time.Now().After(until) {
		return false
	}
	return true
}
