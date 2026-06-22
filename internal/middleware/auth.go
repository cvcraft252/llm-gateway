package middleware

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/cvcraft252/llm-gateway/internal/config"
	"github.com/cvcraft252/llm-gateway/internal/respond"
)

func Auth(cfg *config.Config, next http.HandlerFunc) http.HandlerFunc {
	// Preallocate map capacity based on configured gateway keys
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
		if !validKeys[token] {
			slog.Warn("Unauthorized key attempt", "ip", r.RemoteAddr, "token", token)
			respond.WriteJSONError(w, http.StatusUnauthorized, "Unauthorized key")
			return
		}

		next.ServeHTTP(w, r)
	}
}
