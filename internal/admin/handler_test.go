package admin_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cvcraft252/llm-gateway/internal/admin"
	"github.com/cvcraft252/llm-gateway/internal/db"

	_ "modernc.org/sqlite"
)

func newTestAdmin(t *testing.T) (*admin.Handler, *db.KeyStore, *db.DB) {
	t.Helper()
	database, err := db.Init(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("db.Init: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	ks := db.NewKeyStore(database)
	h := admin.New(ks, database, []string{"admin-secret-key"})
	return h, ks, database
}

func TestAdminAuth(t *testing.T) {
	t.Parallel()

	h, _, _ := newTestAdmin(t)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := h.AuthMiddleware(next)

	tests := []struct {
		name       string
		authHeader string
		wantStatus int
	}{
		{"missing header", "", http.StatusUnauthorized},
		{"invalid format", "Token admin-secret-key", http.StatusUnauthorized},
		{"wrong admin key", "Bearer wrong-key", http.StatusUnauthorized},
		{"gateway key not accepted", "Bearer gw-somekey", http.StatusUnauthorized},
		{"valid admin key", "Bearer admin-secret-key", http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequest(http.MethodPost, "/v1/admin/keys", nil)
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
		})
	}
}

func TestCreateKey(t *testing.T) {
	t.Parallel()

	h, ks, _ := newTestAdmin(t)

	t.Run("create with name", func(t *testing.T) {
		body := strings.NewReader(`{"name":"my-app"}`)
		req := httptest.NewRequest(http.MethodPost, "/v1/admin/keys", body)
		req.Header.Set("Authorization", "Bearer admin-secret-key")
		rec := httptest.NewRecorder()

		h.CreateKey(rec, req)

		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201. Body: %s", rec.Code, rec.Body.String())
		}

		var resp struct {
			Key   string `json:"key"`
			KeyID string `json:"key_id"`
			Name  string `json:"name"`
		}
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if !strings.HasPrefix(resp.Key, "gw-") {
			t.Errorf("key should start with gw-, got %q", resp.Key[:3])
		}
		if resp.KeyID != resp.Key[:12] {
			t.Errorf("key_id = %q, want %q", resp.KeyID, resp.Key[:12])
		}
		if resp.Name != "my-app" {
			t.Errorf("name = %q, want %q", resp.Name, "my-app")
		}

		// Verify the key is in the DB
		ak, err := ks.Lookup(context.Background(), resp.Key)
		if err != nil {
			t.Fatalf("key not found in DB: %v", err)
		}
		if ak.Name != "my-app" {
			t.Errorf("DB name = %q, want %q", ak.Name, "my-app")
		}
	})

	t.Run("create without name", func(t *testing.T) {
		body := strings.NewReader(`{}`)
		req := httptest.NewRequest(http.MethodPost, "/v1/admin/keys", body)
		req.Header.Set("Authorization", "Bearer admin-secret-key")
		rec := httptest.NewRecorder()

		h.CreateKey(rec, req)

		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201", rec.Code)
		}
	})

	t.Run("invalid JSON body", func(t *testing.T) {
		body := strings.NewReader(`{invalid`)
		req := httptest.NewRequest(http.MethodPost, "/v1/admin/keys", body)
		req.Header.Set("Authorization", "Bearer admin-secret-key")
		rec := httptest.NewRecorder()

		h.CreateKey(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", rec.Code)
		}
	})
}

func TestListKeys(t *testing.T) {
	t.Parallel()

	h, ks, _ := newTestAdmin(t)
	ctx := context.Background()

	// Create 2 keys, revoke 1
	_, _ = ks.Create(ctx, "key-one")
	key2, _ := ks.Create(ctx, "key-two")
	_ = ks.Revoke(ctx, key2[:12])

	t.Run("list all", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/admin/keys", nil)
		req.Header.Set("Authorization", "Bearer admin-secret-key")
		rec := httptest.NewRecorder()

		h.ListKeys(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}

		var resp struct {
			Keys []db.APIKey `json:"keys"`
		}
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(resp.Keys) != 2 {
			t.Errorf("key count = %d, want 2", len(resp.Keys))
		}
	})

	t.Run("list active only", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/admin/keys?status=active", nil)
		req.Header.Set("Authorization", "Bearer admin-secret-key")
		rec := httptest.NewRecorder()

		h.ListKeys(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}

		var resp struct {
			Keys []db.APIKey `json:"keys"`
		}
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(resp.Keys) != 1 {
			t.Errorf("active key count = %d, want 1", len(resp.Keys))
		}
		if resp.Keys[0].Status != "active" {
			t.Errorf("status = %q, want active", resp.Keys[0].Status)
		}
	})
}

func TestRevokeKey(t *testing.T) {
	t.Parallel()

	h, ks, _ := newTestAdmin(t)
	ctx := context.Background()

	fullKey, _ := ks.Create(ctx, "to-revoke")
	keyID := fullKey[:12]

	t.Run("revoke existing key", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodDelete, "/v1/admin/keys/"+keyID, nil)
		req.SetPathValue("key_id", keyID)
		req.Header.Set("Authorization", "Bearer admin-secret-key")
		rec := httptest.NewRecorder()

		h.RevokeKey(rec, req)

		if rec.Code != http.StatusNoContent {
			t.Errorf("status = %d, want 204", rec.Code)
		}

		_, err := ks.Lookup(ctx, fullKey)
		if err == nil {
			t.Errorf("key should be revoked")
		}
	})

	t.Run("revoke nonexistent key", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodDelete, "/v1/admin/keys/gw-nonexist", nil)
		req.SetPathValue("key_id", "gw-nonexist")
		req.Header.Set("Authorization", "Bearer admin-secret-key")
		rec := httptest.NewRecorder()

		h.RevokeKey(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", rec.Code)
		}
	})

	t.Run("revoke with empty key_id", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodDelete, "/v1/admin/keys", nil)
		req.SetPathValue("key_id", "")
		req.Header.Set("Authorization", "Bearer admin-secret-key")
		rec := httptest.NewRecorder()

		h.RevokeKey(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", rec.Code)
		}
	})
}

func TestListAuditLogs(t *testing.T) {
	t.Parallel()

	h, _, database := newTestAdmin(t)
	ctx := context.Background()

	// Insert test audit records
	inserts := []struct {
		keyID, model, upstream string
		tokens                 int
	}{
		{"gw-key00001", "deepseek-chat", "deepseek", 100},
		{"gw-key00001", "gpt-4o", "openai", 200},
		{"gw-key00002", "deepseek-chat", "deepseek", 50},
		{"gw-key00003", "llama3", "ollama", 30},
	}
	for _, ins := range inserts {
		_, err := database.Conn().ExecContext(ctx,
			`INSERT INTO audit_logs (key_id, model, upstream, total_tokens, status_code, is_stream, duration_ms, prompt_tokens, completion_tokens) VALUES (?, ?, ?, ?, ?, 0, 10, 5, ?)`,
			ins.keyID, ins.model, ins.upstream, ins.tokens, 200, ins.tokens-5,
		)
		if err != nil {
			t.Fatalf("insert audit: %v", err)
		}
	}

	t.Run("list all logs", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/admin/audit/logs", nil)
		req.Header.Set("Authorization", "Bearer admin-secret-key")
		rec := httptest.NewRecorder()

		h.ListAuditLogs(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}

		var resp struct {
			Logs   []db.AuditLogRecord `json:"logs"`
			Count  int                 `json:"count"`
			Limit  int                 `json:"limit"`
			Offset int                 `json:"offset"`
		}
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Count != 4 {
			t.Errorf("count = %d, want 4", resp.Count)
		}
		// Should be ordered by id DESC (newest first)
		if resp.Logs[0].KeyID != "gw-key00003" {
			t.Errorf("first log key_id = %q, want %q (newest first)", resp.Logs[0].KeyID, "gw-key00003")
		}
	})

	t.Run("filter by key_id", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/admin/audit/logs?key_id=gw-key00001", nil)
		req.Header.Set("Authorization", "Bearer admin-secret-key")
		rec := httptest.NewRecorder()

		h.ListAuditLogs(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}

		var resp struct {
			Logs  []db.AuditLogRecord `json:"logs"`
			Count int                 `json:"count"`
		}
		_ = json.NewDecoder(rec.Body).Decode(&resp)
		if resp.Count != 2 {
			t.Errorf("count = %d, want 2 (filtered by key_id)", resp.Count)
		}
		for _, log := range resp.Logs {
			if log.KeyID != "gw-key00001" {
				t.Errorf("unexpected key_id %q in filtered results", log.KeyID)
			}
		}
	})

	t.Run("filter by model", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/admin/audit/logs?model=deepseek-chat", nil)
		req.Header.Set("Authorization", "Bearer admin-secret-key")
		rec := httptest.NewRecorder()

		h.ListAuditLogs(rec, req)

		var resp struct {
			Count int `json:"count"`
		}
		_ = json.NewDecoder(rec.Body).Decode(&resp)
		if resp.Count != 2 {
			t.Errorf("count = %d, want 2 (deepseek-chat)", resp.Count)
		}
	})

	t.Run("pagination", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/admin/audit/logs?limit=2&offset=0", nil)
		req.Header.Set("Authorization", "Bearer admin-secret-key")
		rec := httptest.NewRecorder()

		h.ListAuditLogs(rec, req)

		var resp struct {
			Logs  []db.AuditLogRecord `json:"logs"`
			Count int                 `json:"count"`
		}
		_ = json.NewDecoder(rec.Body).Decode(&resp)
		if resp.Count != 2 {
			t.Errorf("count = %d, want 2 (limit)", resp.Count)
		}
		if len(resp.Logs) != 2 {
			t.Errorf("logs length = %d, want 2", len(resp.Logs))
		}
	})

	t.Run("default limit when invalid", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/admin/audit/logs?limit=abc", nil)
		req.Header.Set("Authorization", "Bearer admin-secret-key")
		rec := httptest.NewRecorder()

		h.ListAuditLogs(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (invalid limit should default)", rec.Code)
		}
	})
}
