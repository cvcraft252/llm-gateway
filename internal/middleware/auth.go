package middleware

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/cvcraft252/llm-gateway/internal/config"
)

func Auth(cfg *config.Config, next http.HandlerFunc) http.HandlerFunc {
	validKeys := make(map[string]bool)
	for _, k := range cfg.Gateway.Keys {
		validKeys[k] = true
	}

	return func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			slog.Warn("Missing Authorization header", "ip", r.RemoteAddr)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error": "Missing Authorization header"}`))
			return
		}

		parts := strings.Split(authHeader, " ")
		if len(parts) != 2 || strings.ToLower(parts[0]) != "bearer" {
			slog.Warn("Invalid Authorization format", "ip", r.RemoteAddr)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error": "Invalid Authorization header format"}`))
			return
		}

		token := parts[1]
		if !validKeys[token] {
			slog.Warn("Unauthorized key attempt", "ip", r.RemoteAddr, "token", token)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error": "Unauthorized key"}`))
			return
		}

		next.ServeHTTP(w, r)
	}
}
