package admin

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/cvcraft252/llm-gateway/internal/db"
	"github.com/cvcraft252/llm-gateway/internal/respond"
)

// Handler exposes admin HTTP endpoints for API key management and audit
// log querying. It is constructed with a KeyStore, a DB for audit queries,
// and a set of admin keys used for authentication. Admin keys are separate
// from gateway keys — a gateway key cannot access admin endpoints and
// vice versa (least privilege).
type Handler struct {
	keyStore    *db.KeyStore
	database    *db.DB
	validAdmins map[string]bool
}

// New creates an admin Handler. adminKeys are the YAML-configured keys
// allowed to access admin endpoints; they are the trust root and cannot
// be managed via the API itself.
func New(keyStore *db.KeyStore, database *db.DB, adminKeys []string) *Handler {
	valid := make(map[string]bool, len(adminKeys))
	for _, k := range adminKeys {
		valid[k] = true
	}
	return &Handler{
		keyStore:    keyStore,
		database:    database,
		validAdmins: valid,
	}
}

// AuthMiddleware wraps the next handler with admin key validation.
// It checks the Authorization header against the configured admin keys,
// not the gateway keys or the DB-backed keys.
func (h *Handler) AuthMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			respond.WriteJSONError(w, http.StatusUnauthorized, "Missing Authorization header")
			return
		}

		parts := strings.Split(authHeader, " ")
		if len(parts) != 2 || strings.ToLower(parts[0]) != "bearer" {
			respond.WriteJSONError(w, http.StatusUnauthorized, "Invalid Authorization header format")
			return
		}

		if !h.validAdmins[parts[1]] {
			slog.Warn("Unauthorized admin key attempt", "ip", r.RemoteAddr)
			respond.WriteJSONError(w, http.StatusUnauthorized, "Unauthorized admin key")
			return
		}

		next.ServeHTTP(w, r)
	}
}

// CreateKey handles POST /v1/admin/keys.
// Request body: {"name": "optional human-readable label"}
// Response: {"key": "gw-...", "key_id": "gw-a1b2c3d4", "name": "..."}
// The full key is returned only once — callers must store it securely.
func (h *Handler) CreateKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		respond.WriteJSONError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}

	fullKey, err := h.keyStore.Create(r.Context(), body.Name)
	if err != nil {
		slog.Error("Failed to create API key", "error", err)
		respond.WriteJSONError(w, http.StatusInternalServerError, "Failed to create key")
		return
	}

	resp := struct {
		Key   string `json:"key"`
		KeyID string `json:"key_id"`
		Name  string `json:"name"`
	}{
		Key:   fullKey,
		KeyID: fullKey[:12],
		Name:  body.Name,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(resp)

	slog.Info("API key created", "key_id", fullKey[:12], "name", body.Name)
}

// ListKeys handles GET /v1/admin/keys.
// Optional query param: ?status=active or ?status=revoked
// Response: {"keys": [...]}
func (h *Handler) ListKeys(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")

	keys, err := h.keyStore.List(r.Context(), status)
	if err != nil {
		slog.Error("Failed to list API keys", "error", err)
		respond.WriteJSONError(w, http.StatusInternalServerError, "Failed to list keys")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(struct {
		Keys []db.APIKey `json:"keys"`
	}{
		Keys: keys,
	})
}

// RevokeKey handles DELETE /v1/admin/keys/{key_id}.
// Response: 204 No Content on success, 404 if key not found.
func (h *Handler) RevokeKey(w http.ResponseWriter, r *http.Request) {
	keyID := r.PathValue("key_id")
	if keyID == "" {
		respond.WriteJSONError(w, http.StatusBadRequest, "Missing key_id in path")
		return
	}

	err := h.keyStore.Revoke(r.Context(), keyID)
	if errors.Is(err, db.ErrKeyNotFound) {
		respond.WriteJSONError(w, http.StatusNotFound, "Key not found")
		return
	}
	if err != nil {
		slog.Error("Failed to revoke API key", "error", err, "key_id", keyID)
		respond.WriteJSONError(w, http.StatusInternalServerError, "Failed to revoke key")
		return
	}

	w.WriteHeader(http.StatusNoContent)
	slog.Info("API key revoked", "key_id", keyID)
}

// ListAuditLogs handles GET /v1/admin/audit/logs.
// Optional query params: ?key_id=&model=&upstream=&limit=&offset=
// Response: {"logs": [...], "count": N}
func (h *Handler) ListAuditLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	limit, err := strconv.Atoi(q.Get("limit"))
	if err != nil {
		limit = 50 // default
	}
	offset, err := strconv.Atoi(q.Get("offset"))
	if err != nil {
		offset = 0
	}

	auditQ := db.AuditQuery{
		KeyID:    q.Get("key_id"),
		Model:    q.Get("model"),
		Upstream: q.Get("upstream"),
		Limit:    limit,
		Offset:   offset,
	}

	records, err := h.database.QueryAuditLogs(r.Context(), auditQ)
	if err != nil {
		slog.Error("Failed to query audit logs", "error", err)
		respond.WriteJSONError(w, http.StatusInternalServerError, "Failed to query audit logs")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(struct {
		Logs   []db.AuditLogRecord `json:"logs"`
		Count  int                 `json:"count"`
		Limit  int                 `json:"limit"`
		Offset int                 `json:"offset"`
	}{
		Logs:   records,
		Count:  len(records),
		Limit:  limit,
		Offset: offset,
	})
}
