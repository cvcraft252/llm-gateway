package db

import (
	"context"
	"database/sql"
	"fmt"
)

// AuditLogRecord is an audit_logs row as returned by QueryAuditLogs.
// It includes the id and created_at columns that are not in the write-only
// AuditLog struct.
type AuditLogRecord struct {
	ID               int64  `json:"id"`
	CreatedAt        string `json:"created_at"`
	Upstream         string `json:"upstream"`
	KeyID            string `json:"key_id"`
	Model            string `json:"model"`
	StatusCode       int    `json:"status_code"`
	IsStream         bool   `json:"is_stream"`
	DurationMs       int64  `json:"duration_ms"`
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	TotalTokens      int    `json:"total_tokens"`
}

// AuditQuery holds optional filters for querying audit logs.
// Empty fields are not included in the WHERE clause.
type AuditQuery struct {
	KeyID    string
	Model    string
	Upstream string
	Limit    int
	Offset   int
}

// QueryAuditLogs retrieves audit log records matching the given filters.
// All filter fields are parameterized to prevent SQL injection.
// Results are ordered by id DESC (newest first).
// If Limit <= 0, defaults to 50. If Limit > 200, capped at 200.
func (db *DB) QueryAuditLogs(ctx context.Context, q AuditQuery) ([]AuditLogRecord, error) {
	if q.Limit <= 0 {
		q.Limit = 50
	}
	if q.Limit > 200 {
		q.Limit = 200
	}

	// Build WHERE clause dynamically with parameterized placeholders.
	// Column names are hardcoded (not from user input), so they are
	// safe from SQL injection. Only values use ? placeholders.
	where := ""
	args := []any{}
	conditions := []string{}

	if q.KeyID != "" {
		conditions = append(conditions, "key_id = ?")
		args = append(args, q.KeyID)
	}
	if q.Model != "" {
		conditions = append(conditions, "model = ?")
		args = append(args, q.Model)
	}
	if q.Upstream != "" {
		conditions = append(conditions, "upstream = ?")
		args = append(args, q.Upstream)
	}

	if len(conditions) > 0 {
		where = " WHERE " + joinConditions(conditions, " AND ")
	}

	query := `SELECT id, created_at, upstream, key_id, model, status_code, is_stream, duration_ms, prompt_tokens, completion_tokens, total_tokens
		FROM audit_logs` + where + `
		ORDER BY id DESC
		LIMIT ? OFFSET ?`
	args = append(args, q.Limit, q.Offset)

	rows, err := db.conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query audit logs: %w", err)
	}
	defer rows.Close()

	var records []AuditLogRecord
	for rows.Next() {
		var r AuditLogRecord
		var isStream sql.NullInt64
		if err := rows.Scan(&r.ID, &r.CreatedAt, &r.Upstream, &r.KeyID, &r.Model, &r.StatusCode, &isStream, &r.DurationMs, &r.PromptTokens, &r.CompletionTokens, &r.TotalTokens); err != nil {
			return nil, fmt.Errorf("scan audit log row: %w", err)
		}
		r.IsStream = isStream.Valid && isStream.Int64 == 1
		records = append(records, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate audit log rows: %w", err)
	}

	return records, nil
}

func joinConditions(conditions []string, sep string) string {
	if len(conditions) == 0 {
		return ""
	}
	result := conditions[0]
	for _, c := range conditions[1:] {
		result += sep + c
	}
	return result
}
