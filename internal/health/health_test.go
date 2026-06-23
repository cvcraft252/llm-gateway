package health

import (
	"testing"
	"time"
)

func TestTracker_InitialState(t *testing.T) {
	t.Parallel()
	tr := NewTracker(3, 100*time.Millisecond)

	if tr.IsDegraded("upstream-1") {
		t.Error("new tracker should not have any degraded upstreams")
	}
}

func TestTracker_DegradationAndCooldown(t *testing.T) {
	t.Parallel()
	cooldown := 50 * time.Millisecond
	tr := NewTracker(2, cooldown)

	// First failure
	tr.RecordFailure("upstream-1")
	if tr.IsDegraded("upstream-1") {
		t.Error("should not be degraded after 1 failure when threshold is 2")
	}

	// Second failure -> degrades
	tr.RecordFailure("upstream-1")
	if !tr.IsDegraded("upstream-1") {
		t.Error("should be degraded after 2 failures")
	}

	// Check another upstream is unaffected
	if tr.IsDegraded("upstream-2") {
		t.Error("upstream-2 should not be degraded")
	}

	// Wait for cooldown
	time.Sleep(cooldown + 5*time.Millisecond)
	if tr.IsDegraded("upstream-1") {
		t.Error("should recover from degradation after cooldown window")
	}
}

func TestTracker_RecordSuccessResets(t *testing.T) {
	t.Parallel()
	tr := NewTracker(2, 10*time.Second)

	tr.RecordFailure("upstream-1")
	tr.RecordSuccess("upstream-1")

	// Another failure should not degrade it because previous failure was reset by success
	tr.RecordFailure("upstream-1")
	if tr.IsDegraded("upstream-1") {
		t.Error("should not be degraded because success reset the failure counter")
	}
}
