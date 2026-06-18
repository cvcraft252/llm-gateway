package db

import (
	"database/sql"
	"log/slog"

	_ "modernc.org/sqlite"
)

type DB struct {
	conn *sql.DB
}

type AuditLog struct {
	Model            string
	StatusCode       int
	IsStream         bool
	DurationMs       int64
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
}

func Init(path string) (*DB, error) {
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}

	query := `
	CREATE TABLE IF NOT EXISTS audit_logs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		model TEXT,
		status_code INTEGER,
		is_stream INTEGER,
		duration_ms INTEGER,
		prompt_tokens INTEGER,
		completion_tokens INTEGER,
		total_tokens INTEGER
	);`

	if _, err := conn.Exec(query); err != nil {
		conn.Close()
		return nil, err
	}

	return &DB{conn: conn}, nil
}

func (db *DB) Close() error {
	return db.conn.Close()
}

// InsertAsync writes logs in a background goroutine safely.
func (db *DB) InsertAsync(log *AuditLog) {
	// Copy the struct by value to prevent any pointer lifecycle issues
	l := *log

	go func() {
		// Explicitly convert bool to int for SQLite compatibility
		var streamInt int
		if l.IsStream {
			streamInt = 1
		}

		// Log the actual values inside the goroutine for debugging
		slog.Info("Writing to database",
			"model", l.Model,
			"status", l.StatusCode,
			"duration_ms", l.DurationMs,
			"prompt_tokens", l.PromptTokens,
			"completion_tokens", l.CompletionTokens,
		)

		query := `
		INSERT INTO audit_logs (model, status_code, is_stream, duration_ms, prompt_tokens, completion_tokens, total_tokens)
		VALUES (?, ?, ?, ?, ?, ?, ?);`

		_, err := db.conn.Exec(query, l.Model, l.StatusCode, streamInt, l.DurationMs, l.PromptTokens, l.CompletionTokens, l.TotalTokens)
		if err != nil {
			slog.Error("Database insert failed", "error", err)
		}
	}()
}
