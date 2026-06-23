package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"github.com/cvcraft252/llm-gateway/internal/admin"
	"github.com/cvcraft252/llm-gateway/internal/config"
	"github.com/cvcraft252/llm-gateway/internal/db"
	"github.com/cvcraft252/llm-gateway/internal/handler"
	"github.com/cvcraft252/llm-gateway/internal/logger"
	"github.com/cvcraft252/llm-gateway/internal/middleware"
	"github.com/cvcraft252/llm-gateway/internal/router"

	// SQLite driver registered globally at application root
	_ "modernc.org/sqlite"
)

func main() {
	logger.Init()
	slog.Info("Starting LLM Gateway...")

	cfgPath := "config.yaml"
	cfg, err := config.Load(cfgPath)
	if err != nil {
		slog.Error("Failed to load config", "error", err, "path", cfgPath)
		os.Exit(1)
	}
	slog.Info("Configuration loaded successfully",
		"port", cfg.Server.Port,
		"upstreams", len(cfg.Upstreams),
		"timeout", cfg.Routing.Timeout,
	)

	database, err := db.Init("gateway.db")
	if err != nil {
		slog.Error("Failed to initialize database", "error", err)
		os.Exit(1)
	}
	defer database.Close()
	slog.Info("Database initialized successfully")

	keyStore := db.NewKeyStore(database)

	rtr, err := router.New(cfg)
	if err != nil {
		slog.Error("Failed to build router", "error", err)
		os.Exit(1)
	}
	slog.Info("Router initialized", "upstreams", len(cfg.Upstreams))

	mux := http.NewServeMux()

	chatHandler, err := handler.NewChatHandler(database, rtr)
	if err != nil {
		slog.Error("Failed to initialize chat handler", "error", err)
		os.Exit(1)
	}
	authedChatHandler := middleware.Auth(cfg, keyStore, chatHandler)

	mux.HandleFunc("POST /v1/chat/completions", authedChatHandler)

	// Admin endpoints for key management (separate auth from gateway keys)
	adminHandler := admin.New(keyStore, database, cfg.Gateway.AdminKeys)
	mux.HandleFunc("POST /v1/admin/keys", adminHandler.AuthMiddleware(adminHandler.CreateKey))
	mux.HandleFunc("GET /v1/admin/keys", adminHandler.AuthMiddleware(adminHandler.ListKeys))
	mux.HandleFunc("DELETE /v1/admin/keys/{key_id}", adminHandler.AuthMiddleware(adminHandler.RevokeKey))
	mux.HandleFunc("GET /v1/admin/audit/logs", adminHandler.AuthMiddleware(adminHandler.ListAuditLogs))

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status": "ok"}`))
	})

	addr := fmt.Sprintf(":%d", cfg.Server.Port)
	slog.Info("Gateway is listening", "addr", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		slog.Error("Server failed to start", "error", err)
		os.Exit(1)
	}
}
