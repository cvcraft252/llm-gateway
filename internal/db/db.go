package db

import (
	"database/sql"
	"fmt"
	"log/slog"
)

type DB struct {
	conn *sql.DB
}

type AuditLog struct {
	Upstream         string
	KeyID            string
	Model            string
	StatusCode       int
	IsStream         bool
	DurationMs       int64
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
}

func Init(path string) (*DB, error) {
	// DSN pragmas apply to every pooled connection, not just the first:
	//   busy_timeout - concurrent async writers wait instead of failing with SQLITE_BUSY
	//   journal_mode=WAL - allows readers and a single writer concurrently
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open sqlite database at %q: %w", path, err)
	}

	query := `
	CREATE TABLE IF NOT EXISTS audit_logs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		upstream TEXT,
		key_id TEXT,
		model TEXT,
		status_code INTEGER,
		is_stream INTEGER,
		duration_ms INTEGER,
		prompt_tokens INTEGER,
		completion_tokens INTEGER,
		total_tokens INTEGER
	);`

	if _, err := conn.Exec(query); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("failed to initialize database schema: %w", err)
	}

	apiKeysQuery := `
	CREATE TABLE IF NOT EXISTS api_keys (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		key_id TEXT UNIQUE NOT NULL,
		key_hash TEXT NOT NULL,
		name TEXT,
		status TEXT NOT NULL DEFAULT 'active',
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		revoked_at DATETIME
	);`

	if _, err := conn.Exec(apiKeysQuery); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("failed to initialize api_keys schema: %w", err)
	}

	if err := migrateUpstreamColumn(conn); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("failed to migrate audit_logs schema: %w", err)
	}

	if err := migrateKeyIDColumn(conn); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("failed to migrate audit_logs key_id column: %w", err)
	}

	return &DB{conn: conn}, nil
}

// migrateUpstreamColumn adds the upstream column to pre-existing audit_logs
// tables. Idempotent: checks PRAGMA table_info before running ALTER TABLE,
// since SQLite does not support ADD COLUMN IF NOT EXISTS.
func migrateUpstreamColumn(conn *sql.DB) error {
	rows, err := conn.Query("PRAGMA table_info(audit_logs)")
	if err != nil {
		return fmt.Errorf("read schema: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return fmt.Errorf("scan schema row: %w", err)
		}
		if name == "upstream" {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate schema rows: %w", err)
	}

	if _, err := conn.Exec("ALTER TABLE audit_logs ADD COLUMN upstream TEXT"); err != nil {
		return fmt.Errorf("add upstream column: %w", err)
	}
	return nil
}

// migrateKeyIDColumn adds the key_id column for per-key usage tracking.
// Idempotent: checks PRAGMA table_info before running ALTER TABLE.
func migrateKeyIDColumn(conn *sql.DB) error {
	rows, err := conn.Query("PRAGMA table_info(audit_logs)")
	if err != nil {
		return fmt.Errorf("read schema: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return fmt.Errorf("scan schema row: %w", err)
		}
		if name == "key_id" {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate schema rows: %w", err)
	}

	if _, err := conn.Exec("ALTER TABLE audit_logs ADD COLUMN key_id TEXT"); err != nil {
		return fmt.Errorf("add key_id column: %w", err)
	}
	return nil
}

func (db *DB) Close() error {
	return db.conn.Close()
}

// Conn returns the underlying *sql.DB, allowing tests and tooling to run
// read-only queries against the audit store.
func (db *DB) Conn() *sql.DB {
	return db.conn
}

// InsertAsync writes logs in a background goroutine safely with panic recovery.
func (db *DB) InsertAsync(log *AuditLog) {
	// Copy the struct by value to prevent any pointer lifecycle issues
	l := *log

	go func() {
		// Recover guard to prevent background panic from crashing the entire server
		defer func() {
			if r := recover(); r != nil {
				slog.Error("Recovered from panic in database insert goroutine", "panic", r)
			}
		}()

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
		INSERT INTO audit_logs (upstream, key_id, model, status_code, is_stream, duration_ms, prompt_tokens, completion_tokens, total_tokens)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?);`

		_, err := db.conn.Exec(
			query,
			l.Upstream,
			l.KeyID,
			l.Model,
			l.StatusCode,
			streamInt,
			l.DurationMs,
			l.PromptTokens,
			l.CompletionTokens,
			l.TotalTokens,
		)
		if err != nil {
			slog.Error("Database insert failed", "error", err)
		}
	}()
}
