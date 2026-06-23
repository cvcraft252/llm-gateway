package db_test

import (
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/cvcraft252/llm-gateway/internal/db"

	_ "modernc.org/sqlite"
)

func TestInit(t *testing.T) {
	t.Parallel()

	t.Run("creates database file and schema with upstream column", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "audit.db")

		database, err := db.Init(path)
		if err != nil {
			t.Fatalf("Init: unexpected error: %v", err)
		}
		defer database.Close()

		rows, err := database.Conn().Query("SELECT name FROM sqlite_master WHERE type='table' AND name='audit_logs'")
		if err != nil {
			t.Fatalf("query schema: %v", err)
		}
		defer rows.Close()

		var tableName string
		if !rows.Next() {
			t.Fatalf("audit_logs table not created")
		}
		if err := rows.Scan(&tableName); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if tableName != "audit_logs" {
			t.Errorf("table name = %q, want %q", tableName, "audit_logs")
		}
		if rows.Next() {
			t.Errorf("expected single audit_logs table, got extra rows")
		}

		if !columnExists(t, database, "audit_logs", "upstream") {
			t.Errorf("upstream column missing from audit_logs")
		}
	})

	t.Run("idempotent - init twice does not error", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "audit.db")

		d1, err := db.Init(path)
		if err != nil {
			t.Fatalf("Init first: %v", err)
		}
		if err := d1.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		d2, err := db.Init(path)
		if err != nil {
			t.Fatalf("Init second: %v", err)
		}
		defer d2.Close()

		if !columnExists(t, d2, "audit_logs", "upstream") {
			t.Errorf("upstream column missing after second Init")
		}
	})
}

func TestInit_MigratesLegacySchema(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "audit.db")

	legacyDSN := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	legacyConn, err := sql.Open("sqlite", legacyDSN)
	if err != nil {
		t.Fatalf("open legacy conn: %v", err)
	}
	_, err = legacyConn.Exec(`
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
	);`)
	if err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}
	_, err = legacyConn.Exec(`INSERT INTO audit_logs (model, status_code, is_stream) VALUES ('legacy-model', 200, 0)`)
	if err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	if err = legacyConn.Close(); err != nil {
		t.Fatalf("close legacy conn: %v", err)
	}

	database, err := db.Init(path)
	if err != nil {
		t.Fatalf("Init should migrate legacy schema: %v", err)
	}
	defer database.Close()

	if !columnExists(t, database, "audit_logs", "upstream") {
		t.Errorf("upstream column not added by migration")
	}

	if !columnExists(t, database, "audit_logs", "key_id") {
		t.Errorf("key_id column not added by migration")
	}

	var legacyCount int
	if err := database.Conn().QueryRow("SELECT COUNT(*) FROM audit_logs WHERE model = 'legacy-model'").Scan(&legacyCount); err != nil {
		t.Fatalf("query legacy row: %v", err)
	}
	if legacyCount != 1 {
		t.Errorf("legacy row count = %d, want 1 (migration must preserve data)", legacyCount)
	}

	var legacyUpstream sql.NullString
	if err := database.Conn().QueryRow("SELECT upstream FROM audit_logs WHERE model = 'legacy-model'").Scan(&legacyUpstream); err != nil {
		t.Fatalf("query legacy upstream: %v", err)
	}
	if legacyUpstream.Valid {
		t.Errorf("legacy row upstream = %q, want NULL (legacy rows have no upstream)", legacyUpstream.String)
	}

	var legacyKeyID sql.NullString
	if err := database.Conn().QueryRow("SELECT key_id FROM audit_logs WHERE model = 'legacy-model'").Scan(&legacyKeyID); err != nil {
		t.Fatalf("query legacy key_id: %v", err)
	}
	if legacyKeyID.Valid {
		t.Errorf("legacy row key_id = %q, want NULL (legacy rows have no key_id)", legacyKeyID.String)
	}
}

func TestInit_InvalidPath(t *testing.T) {
	t.Parallel()

	invalidPath := filepath.Join(t.TempDir(), "nonexistent-dir", "audit.db")
	_, err := db.Init(invalidPath)
	if err == nil {
		t.Fatalf("Init: expected error for invalid path, got nil")
	}
}

func TestInsertAsync(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "audit.db")
	database, err := db.Init(path)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer database.Close()

	logs := []db.AuditLog{
		{Upstream: "deepseek", KeyID: "gw-a1b2c3d4", Model: "deepseek-chat", StatusCode: 200, IsStream: true, DurationMs: 150, PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30},
		{Upstream: "deepseek", KeyID: "gw-a1b2c3d4", Model: "deepseek-chat", StatusCode: 200, IsStream: false, DurationMs: 80, PromptTokens: 5, CompletionTokens: 15, TotalTokens: 20},
		{Upstream: "openai", KeyID: "gw-e5f6g7h8", Model: "gpt-4", StatusCode: 500, IsStream: false, DurationMs: 5, PromptTokens: 0, CompletionTokens: 0, TotalTokens: 0},
		{Upstream: "", KeyID: "", Model: "", StatusCode: 400, IsStream: false, DurationMs: 1, PromptTokens: 0, CompletionTokens: 0, TotalTokens: 0},
	}

	var wg sync.WaitGroup
	for _, l := range logs {
		wg.Add(1)
		go func(l db.AuditLog) {
			defer wg.Done()
			database.InsertAsync(&l)
		}(l)
	}
	wg.Wait()

	if err := waitForRows(database, len(logs), 2*time.Second); err != nil {
		t.Fatalf("rows not persisted: %v", err)
	}

	rows, err := database.Conn().Query(`SELECT upstream, key_id, model, status_code, is_stream, duration_ms, prompt_tokens, completion_tokens, total_tokens FROM audit_logs ORDER BY id`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	got := []db.AuditLog{}
	for rows.Next() {
		var l db.AuditLog
		var isStream int
		if err := rows.Scan(&l.Upstream, &l.KeyID, &l.Model, &l.StatusCode, &isStream, &l.DurationMs, &l.PromptTokens, &l.CompletionTokens, &l.TotalTokens); err != nil {
			t.Fatalf("scan: %v", err)
		}
		l.IsStream = isStream == 1
		got = append(got, l)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}

	if len(got) != len(logs) {
		t.Fatalf("row count = %d, want %d", len(got), len(logs))
	}

	want := map[string]db.AuditLog{
		"deepseek:gw-a1b2c3d4:deepseek-chat:200:true":  logs[0],
		"deepseek:gw-a1b2c3d4:deepseek-chat:200:false": logs[1],
		"openai:gw-e5f6g7h8:gpt-4:500:false":           logs[2],
		":::400:false":                                 logs[3],
	}
	for _, l := range got {
		key := l.Upstream + ":" + l.KeyID + ":" + l.Model + ":" + itoa(l.StatusCode) + ":" + boolStr(l.IsStream)
		exp, ok := want[key]
		if !ok {
			t.Errorf("unexpected row: %+v", l)
			continue
		}
		if l.DurationMs != exp.DurationMs {
			t.Errorf("DurationMs for %s = %d, want %d", key, l.DurationMs, exp.DurationMs)
		}
		if l.PromptTokens != exp.PromptTokens {
			t.Errorf("PromptTokens for %s = %d, want %d", key, l.PromptTokens, exp.PromptTokens)
		}
		if l.CompletionTokens != exp.CompletionTokens {
			t.Errorf("CompletionTokens for %s = %d, want %d", key, l.CompletionTokens, exp.CompletionTokens)
		}
		if l.TotalTokens != exp.TotalTokens {
			t.Errorf("TotalTokens for %s = %d, want %d", key, l.TotalTokens, exp.TotalTokens)
		}
	}
}

func TestInsertAsync_StructCopiedByValue(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "audit.db")
	database, err := db.Init(path)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer database.Close()

	l := db.AuditLog{Upstream: "deepseek", KeyID: "gw-test1234", Model: "test-model", StatusCode: 200, IsStream: false, DurationMs: 42, PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3}
	database.InsertAsync(&l)

	l.Model = "mutated-after-call"
	l.StatusCode = 999
	l.Upstream = "mutated-upstream"
	l.KeyID = "mutated-key-id"

	if err := waitForRows(database, 1, 2*time.Second); err != nil {
		t.Fatalf("rows not persisted: %v", err)
	}

	var gotUpstream, gotKeyID, gotModel string
	var gotStatus int
	err = database.Conn().QueryRow("SELECT upstream, key_id, model, status_code FROM audit_logs ORDER BY id LIMIT 1").Scan(&gotUpstream, &gotKeyID, &gotModel, &gotStatus)
	if err != nil {
		t.Fatalf("QueryRow: %v", err)
	}
	if gotModel != "test-model" {
		t.Errorf("Model = %q, want %q (struct must be copied by value, not mutated after call)", gotModel, "test-model")
	}
	if gotUpstream != "deepseek" {
		t.Errorf("Upstream = %q, want %q (struct must be copied by value, not mutated after call)", gotUpstream, "deepseek")
	}
	if gotKeyID != "gw-test1234" {
		t.Errorf("KeyID = %q, want %q (struct must be copied by value, not mutated after call)", gotKeyID, "gw-test1234")
	}
	if gotStatus != 200 {
		t.Errorf("StatusCode = %d, want %d", gotStatus, 200)
	}
}

func TestClose(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "audit.db")
	database, err := db.Init(path)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Errorf("Close: unexpected error: %v", err)
	}
}

func columnExists(t *testing.T, database *db.DB, table, column string) bool {
	t.Helper()
	rows, err := database.Conn().Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		t.Fatalf("PRAGMA table_info: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if name == column {
			return true
		}
	}
	return false
}

func waitForRows(database *db.DB, want int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var count int
		if err := database.Conn().QueryRow("SELECT COUNT(*) FROM audit_logs").Scan(&count); err != nil {
			return err
		}
		if count >= want {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	var count int
	_ = database.Conn().QueryRow("SELECT COUNT(*) FROM audit_logs").Scan(&count)
	return &timeoutError{want: want, got: count}
}

type timeoutError struct {
	want, got int
}

func (e *timeoutError) Error() string {
	return "timed out waiting for rows: got " + itoa(e.got) + ", want " + itoa(e.want)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
