package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"github.com/cvcraft252/llm-gateway/internal/config"
	"github.com/cvcraft252/llm-gateway/internal/db"
	"github.com/cvcraft252/llm-gateway/internal/handler"
	"github.com/cvcraft252/llm-gateway/internal/logger"
	"github.com/cvcraft252/llm-gateway/internal/middleware"
)

func main() {
	logger.Init()
	slog.Info("Starting LLM Gateway MVP...")

	cfgPath := "config.yaml"
	cfg, err := config.Load(cfgPath)
	if err != nil {
		slog.Error("Failed to load config", "error", err, "path", cfgPath)
		os.Exit(1)
	}
	slog.Info("Configuration loaded successfully", "port", cfg.Server.Port)

	// Initialize SQLite Database
	database, err := db.Init("gateway.db")
	if err != nil {
		slog.Error("Failed to initialize database", "error", err)
		os.Exit(1)
	}
	defer database.Close()
	slog.Info("Database initialized successfully")

	mux := http.NewServeMux()

	// Pass database connection to chat handler
	chatHandler := handler.NewChatHandler(cfg, database)
	authedChatHandler := middleware.Auth(cfg, chatHandler)

	mux.HandleFunc("POST /v1/chat/completions", authedChatHandler)

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status": "ok"}`))
	})

	addr := fmt.Sprintf(":%d", cfg.Server.Port)
	slog.Info("Gateway is listening", "addr", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		slog.Error("Server failed to start", "error", err)
		os.Exit(1)
	}
}
