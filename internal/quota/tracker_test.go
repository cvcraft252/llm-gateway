package quota_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cvcraft252/llm-gateway/internal/db"
	"github.com/cvcraft252/llm-gateway/internal/middleware"
	"github.com/cvcraft252/llm-gateway/internal/quota"

	_ "modernc.org/sqlite"
)

func newTestQuota(t *testing.T, dailyLimit int) (*quota.Tracker, *db.DB) {
	t.Helper()
	database, err := db.Init(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("db.Init: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	return quota.New(database.Conn(), dailyLimit), database
}

func setKeyID(ctx context.Context, keyID string) context.Context {
	return context.WithValue(ctx, middleware.CtxKeyKeyID, keyID)
}

func insertAudit(t *testing.T, database *db.DB, keyID string, tokens int) {
	t.Helper()
	_, err := database.Conn().Exec(
		`INSERT INTO audit_logs (key_id, total_tokens, status_code) VALUES (?, ?, ?)`,
		keyID, tokens, 200,
	)
	if err != nil {
		t.Fatalf("insert audit: %v", err)
	}
}

func TestTracker_UsedToday_Empty(t *testing.T) {
	t.Parallel()
	tr, _ := newTestQuota(t, 1000)

	used, err := tr.UsedToday("gw-newkey12")
	if err != nil {
		t.Fatalf("UsedToday: %v", err)
	}
	if used != 0 {
		t.Errorf("used = %d, want 0 for key with no records", used)
	}
}

func TestTracker_UsedToday_WithRecords(t *testing.T) {
	t.Parallel()
	tr, database := newTestQuota(t, 1000)

	insertAudit(t, database, "gw-key00001", 100)
	insertAudit(t, database, "gw-key00001", 200)
	insertAudit(t, database, "gw-key00001", 50)

	used, err := tr.UsedToday("gw-key00001")
	if err != nil {
		t.Fatalf("UsedToday: %v", err)
	}
	if used != 350 {
		t.Errorf("used = %d, want 350 (100+200+50)", used)
	}
}

func TestTracker_UsedToday_PerKeyIsolation(t *testing.T) {
	t.Parallel()
	tr, database := newTestQuota(t, 1000)

	insertAudit(t, database, "gw-keyA0001", 500)
	insertAudit(t, database, "gw-keyB0001", 200)

	usedA, _ := tr.UsedToday("gw-keyA0001")
	usedB, _ := tr.UsedToday("gw-keyB0001")

	if usedA != 500 {
		t.Errorf("keyA used = %d, want 500", usedA)
	}
	if usedB != 200 {
		t.Errorf("keyB used = %d, want 200", usedB)
	}
}

func TestTracker_Middleware_Disabled(t *testing.T) {
	t.Parallel()
	tr, _ := newTestQuota(t, 0) // disabled

	next := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := tr.Middleware(next)

	for i := 0; i < 10; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		req = req.WithContext(setKeyID(req.Context(), "gw-anykey01"))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("request %d: status = %d, want 200 (disabled)", i+1, rec.Code)
		}
	}
}

func TestTracker_Middleware_BelowQuota(t *testing.T) {
	t.Parallel()
	tr, database := newTestQuota(t, 1000)
	insertAudit(t, database, "gw-user0001", 500) // 500/1000 used

	next := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := tr.Middleware(next)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req = req.WithContext(setKeyID(req.Context(), "gw-user0001"))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (below quota)", rec.Code)
	}
}

func TestTracker_Middleware_OverQuota(t *testing.T) {
	t.Parallel()
	tr, database := newTestQuota(t, 1000)
	insertAudit(t, database, "gw-over00001", 1000) // at limit

	next := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		t.Errorf("next should not be called when over quota")
	})
	handler := tr.Middleware(next)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req = req.WithContext(setKeyID(req.Context(), "gw-over00001"))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429 (over quota)", rec.Code)
	}
}

func TestTracker_Middleware_YAMLKeyExempt(t *testing.T) {
	t.Parallel()
	tr, database := newTestQuota(t, 100)
	// Even if "yaml" somehow has usage records, it should be exempt
	insertAudit(t, database, "yaml", 99999)

	next := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := tr.Middleware(next)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req = req.WithContext(setKeyID(req.Context(), "yaml"))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (yaml keys exempt from quota)", rec.Code)
	}
}

func TestTracker_Middleware_NoKeyIDPasses(t *testing.T) {
	t.Parallel()
	tr, _ := newTestQuota(t, 1)

	next := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := tr.Middleware(next)

	// No key_id in context
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (no key_id = no quota check)", rec.Code)
	}
}

func TestTracker_Middleware_PerKeyIsolation(t *testing.T) {
	t.Parallel()
	tr, database := newTestQuota(t, 500)

	// keyA is over quota, keyB is not
	insertAudit(t, database, "gw-keyA0001", 500)
	insertAudit(t, database, "gw-keyB0001", 100)

	next := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := tr.Middleware(next)

	// keyA should be blocked
	reqA := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	reqA = reqA.WithContext(setKeyID(reqA.Context(), "gw-keyA0001"))
	recA := httptest.NewRecorder()
	handler.ServeHTTP(recA, reqA)
	if recA.Code != http.StatusTooManyRequests {
		t.Errorf("keyA: status = %d, want 429 (over quota)", recA.Code)
	}

	// keyB should pass
	reqB := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	reqB = reqB.WithContext(setKeyID(reqB.Context(), "gw-keyB0001"))
	recB := httptest.NewRecorder()
	handler.ServeHTTP(recB, reqB)
	if recB.Code != http.StatusOK {
		t.Errorf("keyB: status = %d, want 200 (below quota)", recB.Code)
	}
}
