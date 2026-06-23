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
	h := admin.New(ks, []string{"admin-secret-key"})
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
