package db_test

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/cvcraft252/llm-gateway/internal/db"

	_ "modernc.org/sqlite"
)

func TestInit(t *testing.T) {
	t.Parallel()

	t.Run("creates database file and schema", func(t *testing.T) {
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
	})
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
		{Model: "deepseek-chat", StatusCode: 200, IsStream: true, DurationMs: 150, PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30},
		{Model: "deepseek-chat", StatusCode: 200, IsStream: false, DurationMs: 80, PromptTokens: 5, CompletionTokens: 15, TotalTokens: 20},
		{Model: "gpt-4", StatusCode: 500, IsStream: false, DurationMs: 5, PromptTokens: 0, CompletionTokens: 0, TotalTokens: 0},
		{Model: "", StatusCode: 400, IsStream: false, DurationMs: 1, PromptTokens: 0, CompletionTokens: 0, TotalTokens: 0},
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

	rows, err := database.Conn().Query(`SELECT model, status_code, is_stream, duration_ms, prompt_tokens, completion_tokens, total_tokens FROM audit_logs ORDER BY id`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	got := []db.AuditLog{}
	for rows.Next() {
		var l db.AuditLog
		var isStream int
		if err := rows.Scan(&l.Model, &l.StatusCode, &isStream, &l.DurationMs, &l.PromptTokens, &l.CompletionTokens, &l.TotalTokens); err != nil {
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
		"deepseek-chat:200:true":  logs[0],
		"deepseek-chat:200:false": logs[1],
		"gpt-4:500:false":         logs[2],
		":400:false":              logs[3],
	}
	for _, l := range got {
		key := l.Model + ":" + itoa(l.StatusCode) + ":" + boolStr(l.IsStream)
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

	l := db.AuditLog{Model: "test-model", StatusCode: 200, IsStream: false, DurationMs: 42, PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3}
	database.InsertAsync(&l)

	l.Model = "mutated-after-call"
	l.StatusCode = 999

	if err := waitForRows(database, 1, 2*time.Second); err != nil {
		t.Fatalf("rows not persisted: %v", err)
	}

	var gotModel string
	var gotStatus int
	err = database.Conn().QueryRow("SELECT model, status_code FROM audit_logs ORDER BY id LIMIT 1").Scan(&gotModel, &gotStatus)
	if err != nil {
		t.Fatalf("QueryRow: %v", err)
	}
	if gotModel != "test-model" {
		t.Errorf("Model = %q, want %q (struct must be copied by value, not mutated after call)", gotModel, "test-model")
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
