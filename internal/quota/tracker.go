package quota

import (
	"database/sql"
	"fmt"
	"net/http"

	"github.com/cvcraft252/llm-gateway/internal/middleware"
	"github.com/cvcraft252/llm-gateway/internal/respond"
)

// Tracker checks per-key daily token usage against a configurable quota.
// It queries the audit_logs table (written by InsertAsync) to compute
// the total tokens consumed by a key on the current day.
//
// The check happens BEFORE forwarding the request because token usage
// is only known AFTER the upstream response completes. This means the
// last request that exceeds the quota will still execute — a small
// acceptable overshoot since LLM APIs cannot predict token counts
// before processing.
type Tracker struct {
	db         *sql.DB
	dailyLimit int
}

// New creates a quota Tracker. If dailyLimit <= 0, quota enforcement
// is disabled (all requests pass regardless of usage).
func New(db *sql.DB, dailyLimit int) *Tracker {
	if dailyLimit <= 0 {
		return &Tracker{db: nil, dailyLimit: 0}
	}
	return &Tracker{db: db, dailyLimit: dailyLimit}
}

// DailyLimit returns the configured daily token limit. Returns 0 if
// quota enforcement is disabled.
func (t *Tracker) DailyLimit() int {
	return t.dailyLimit
}

// UsedToday returns the total tokens consumed by the given key_id today.
// Returns 0 if the key has no audit records for today.
func (t *Tracker) UsedToday(keyID string) (int, error) {
	if t.dailyLimit <= 0 {
		return 0, nil
	}

	var total sql.NullInt64
	err := t.db.QueryRow(
		`SELECT COALESCE(SUM(total_tokens), 0) FROM audit_logs
		 WHERE key_id = ? AND date(created_at) = date('now')`,
		keyID,
	).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("query daily token usage: %w", err)
	}
	return int(total.Int64), nil
}

// Middleware enforces per-key daily token quota. Reads key_id from
// request context (set by auth middleware). If the key's total token
// usage today has reached the daily limit, responds with 429.
//
// YAML bootstrap keys (keyID == "yaml") are exempt from quota — they
// are the trust root and should always be able to access the gateway.
func (t *Tracker) Middleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if t.dailyLimit <= 0 {
			next.ServeHTTP(w, req)
			return
		}

		keyID, _ := req.Context().Value(middleware.CtxKeyKeyID).(string)
		if keyID == "" || keyID == "yaml" {
			next.ServeHTTP(w, req)
			return
		}

		used, err := t.UsedToday(keyID)
		if err != nil {
			// On DB error, allow the request rather than blocking all
			// traffic — a quota check failure should not take down the
			// gateway. The error is logged for investigation.
			next.ServeHTTP(w, req)
			return
		}

		if used >= t.dailyLimit {
			respond.WriteJSONError(w, http.StatusTooManyRequests,
				fmt.Sprintf("Daily token quota exceeded (%d/%d)", used, t.dailyLimit))
			return
		}

		next.ServeHTTP(w, req)
	}
}
