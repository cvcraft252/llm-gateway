package middleware

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/cvcraft252/llm-gateway/internal/config"
	"github.com/cvcraft252/llm-gateway/internal/db"
	"github.com/cvcraft252/llm-gateway/internal/respond"
)

// CtxKeyKeyID is the context key for the authenticated API key's key_id.
// Set by Auth middleware, read by handler to attribute audit logs to keys.
// YAML bootstrap keys use the sentinel value "yaml" since they have no DB id.
type CtxKeyType string

const CtxKeyKeyID CtxKeyType = "key_id"

// Auth builds an authentication middleware that validates Bearer tokens
// against two sources in order:
//  1. YAML config keys (static bootstrap keys, always valid)
//  2. DB-backed keys via KeyStore.Lookup (runtime-managed, revocable)
//
// On success, the key's identity is stored in the request context under
// CtxKeyKeyID for downstream middleware (rate limiter, quota) and the
// handler (audit log attribution).
func Auth(cfg *config.Config, keyStore *db.KeyStore, next http.HandlerFunc) http.HandlerFunc {
	validKeys := make(map[string]bool, len(cfg.Gateway.Keys))
	for _, k := range cfg.Gateway.Keys {
		validKeys[k] = true
	}

	return func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			slog.Warn("Missing Authorization header", "ip", r.RemoteAddr)
			respond.WriteJSONError(w, http.StatusUnauthorized, "Missing Authorization header")
			return
		}

		parts := strings.Split(authHeader, " ")
		if len(parts) != 2 || strings.ToLower(parts[0]) != "bearer" {
			slog.Warn("Invalid Authorization format", "ip", r.RemoteAddr)
			respond.WriteJSONError(w, http.StatusUnauthorized, "Invalid Authorization header format")
			return
		}

		token := parts[1]

		// Check YAML bootstrap keys first (fast path, no DB lookup)
		if validKeys[token] {
			ctx := context.WithValue(r.Context(), CtxKeyKeyID, "yaml")
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		// Check DB-backed keys if a KeyStore is configured
		if keyStore != nil {
			ak, err := keyStore.Lookup(r.Context(), token)
			if err == nil && ak != nil {
				ctx := context.WithValue(r.Context(), CtxKeyKeyID, ak.KeyID)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
		}

		slog.Warn("Unauthorized key attempt", "ip", r.RemoteAddr, "token_prefix", safePrefix(token))
		respond.WriteJSONError(w, http.StatusUnauthorized, "Unauthorized key")
	}
}

// safePrefix returns the first 12 characters of a token for logging,
// enough to identify the key_id without exposing the full key.
func safePrefix(token string) string {
	if len(token) <= 12 {
		return "***"
	}
	return token[:12] + "***"
}
