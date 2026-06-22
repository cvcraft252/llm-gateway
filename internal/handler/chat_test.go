package handler

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cvcraft252/llm-gateway/internal/config"
	"github.com/cvcraft252/llm-gateway/internal/db"

	_ "modernc.org/sqlite"
)

func TestByteBufferPool(t *testing.T) {
	t.Parallel()

	pool := newByteBufferPool()

	buf := pool.Get()
	if cap(buf) != 32*1024 {
		t.Errorf("Get: capacity = %d, want %d", cap(buf), 32*1024)
	}

	original := pool.Get()
	original[0] = 'x'
	pool.Put(original)

	reused := pool.Get()
	if cap(reused) != 32*1024 {
		t.Errorf("reused capacity = %d, want %d", cap(reused), 32*1024)
	}
}

func TestByteBufferPool_Concurrent(t *testing.T) {
	t.Parallel()

	pool := newByteBufferPool()
	var wg sync.WaitGroup

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b := pool.Get()
			b[0] = 0
			pool.Put(b)
		}()
	}
	wg.Wait()
}

func TestParseLine(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		line      string
		wantUsage *Usage
		wantSet   bool
	}{
		{
			name:      "stream chunk with usage",
			line:      `data: {"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}}`,
			wantUsage: &Usage{PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30},
			wantSet:   true,
		},
		{
			name:    "data DONE marker",
			line:    "data: [DONE]",
			wantSet: false,
		},
		{
			name:    "non-data line",
			line:    `event: ping`,
			wantSet: false,
		},
		{
			name:    "empty line",
			line:    ``,
			wantSet: false,
		},
		{
			name:    "invalid json in data payload",
			line:    `data: {invalid`,
			wantSet: false,
		},
		{
			name:      "data payload without usage field",
			line:      `data: {"choices":[{"delta":{"content":"hello"}}]}`,
			wantUsage: nil,
			wantSet:   false,
		},
		{
			name:      "multiple usage chunks - last wins",
			line:      `data: {"usage":{"prompt_tokens":99,"completion_tokens":99,"total_tokens":99}}`,
			wantUsage: &Usage{PromptTokens: 99, CompletionTokens: 99, TotalTokens: 99},
			wantSet:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			o := &observedBody{}
			o.parseLine([]byte(tt.line))

			if !tt.wantSet {
				if o.finalUsage != nil {
					t.Errorf("finalUsage = %+v, want nil", o.finalUsage)
				}
				return
			}
			if o.finalUsage == nil {
				t.Fatalf("finalUsage = nil, want %+v", tt.wantUsage)
			}
			if *o.finalUsage != *tt.wantUsage {
				t.Errorf("finalUsage = %+v, want %+v", *o.finalUsage, *tt.wantUsage)
			}
		})
	}
}

func TestParseLine_LastUsageWins(t *testing.T) {
	t.Parallel()

	o := &observedBody{}
	o.parseLine([]byte(`data: {"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`))
	o.parseLine([]byte(`data: {"choices":[{"delta":{"content":"hi"}}]}`))
	o.parseLine([]byte(`data: {"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}}`))
	o.parseLine([]byte(`data: [DONE]`))

	if o.finalUsage == nil {
		t.Fatalf("finalUsage = nil, want last non-DONE usage")
	}
	if o.finalUsage.PromptTokens != 10 || o.finalUsage.CompletionTokens != 20 || o.finalUsage.TotalTokens != 30 {
		t.Errorf("finalUsage = %+v, want last usage (10/20/30)", *o.finalUsage)
	}
}

func TestProcessBytes_SplitsOnNewline(t *testing.T) {
	t.Parallel()

	o := &observedBody{}
	input := []byte("data: {\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":7,\"total_tokens\":12}}\n")
	o.processBytes(input)

	if o.finalUsage == nil {
		t.Fatalf("finalUsage = nil after processBytes")
	}
	if o.finalUsage.TotalTokens != 12 {
		t.Errorf("TotalTokens = %d, want 12", o.finalUsage.TotalTokens)
	}
	if len(o.lineBuf) != 0 {
		t.Errorf("lineBuf = %d bytes, want 0 (fully consumed)", len(o.lineBuf))
	}
}

func TestProcessBytes_PartialLineAccumulates(t *testing.T) {
	t.Parallel()

	o := &observedBody{}
	part1 := []byte(`data: {"usage":{"prompt_tokens":1`)
	part2 := []byte(`,"completion_tokens":2,"total_tokens":3}}` + "\n")

	o.processBytes(part1)
	if o.finalUsage != nil {
		t.Fatalf("finalUsage set before newline, got %+v", o.finalUsage)
	}
	if len(o.lineBuf) == 0 {
		t.Fatalf("lineBuf should accumulate partial line")
	}

	o.processBytes(part2)
	if o.finalUsage == nil {
		t.Fatalf("finalUsage = nil after completing line")
	}
	if o.finalUsage.TotalTokens != 3 {
		t.Errorf("TotalTokens = %d, want 3", o.finalUsage.TotalTokens)
	}
}

func TestObservedBody_Read(t *testing.T) {
	t.Parallel()

	src := `data: {"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}
data: [DONE]
`
	o := &observedBody{
		ReadCloser: io.NopCloser(strings.NewReader(src)),
	}

	buf := make([]byte, 32*1024)
	n, err := o.Read(buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if n == 0 {
		t.Fatalf("Read: n = 0, want > 0")
	}
	if o.finalUsage == nil || o.finalUsage.TotalTokens != 2 {
		t.Errorf("finalUsage = %+v, want TotalTokens=2", o.finalUsage)
	}

	n2, err := o.Read(buf)
	if err != io.EOF {
		t.Fatalf("Read second: err = %v, want io.EOF", err)
	}
	if n2 != 0 {
		t.Errorf("Read second: n = %d, want 0 (EOF)", n2)
	}
}

func TestObservedBody_Close_PersistsAudit(t *testing.T) {
	t.Parallel()

	path := t.TempDir() + "/audit.db"
	database, err := db.Init(path)
	if err != nil {
		t.Fatalf("db.Init: %v", err)
	}
	defer database.Close()

	src := `data: {"usage":{"prompt_tokens":5,"completion_tokens":8,"total_tokens":13}}
data: [DONE]
`
	o := &observedBody{
		ReadCloser: io.NopCloser(strings.NewReader(src)),
		start:      time.Now().Add(-50 * time.Millisecond),
		model:      "test-model",
		isStream:   true,
		database:   database,
		statusCode: 200,
	}

	buf := make([]byte, 32*1024)
	_, _ = o.Read(buf)

	if err := o.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var count int
		if err := database.Conn().QueryRow("SELECT COUNT(*) FROM audit_logs").Scan(&count); err != nil {
			t.Fatalf("count: %v", err)
		}
		if count == 1 {
			var model string
			var status, isStream, prompt, completion, total int
			if err := database.Conn().QueryRow("SELECT model, status_code, is_stream, prompt_tokens, completion_tokens, total_tokens FROM audit_logs ORDER BY id LIMIT 1").Scan(&model, &status, &isStream, &prompt, &completion, &total); err != nil {
				t.Fatalf("scan: %v", err)
			}
			if model != "test-model" {
				t.Errorf("model = %q, want %q", model, "test-model")
			}
			if status != 200 {
				t.Errorf("status = %d, want 200", status)
			}
			if isStream != 1 {
				t.Errorf("is_stream = %d, want 1", isStream)
			}
			if prompt != 5 || completion != 8 || total != 13 {
				t.Errorf("tokens = %d/%d/%d, want 5/8/13", prompt, completion, total)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("audit row not persisted within timeout")
}

func TestObservedBody_Close_NoUsage(t *testing.T) {
	t.Parallel()

	path := t.TempDir() + "/audit.db"
	database, err := db.Init(path)
	if err != nil {
		t.Fatalf("db.Init: %v", err)
	}
	defer database.Close()

	o := &observedBody{
		ReadCloser: io.NopCloser(strings.NewReader("raw body no sse")),
		start:      time.Now(),
		model:      "no-usage-model",
		isStream:   false,
		database:   database,
		statusCode: 200,
	}

	if err := o.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var count int
		if err := database.Conn().QueryRow("SELECT COUNT(*) FROM audit_logs").Scan(&count); err != nil {
			t.Fatalf("count: %v", err)
		}
		if count == 1 {
			var prompt, completion, total int
			if err := database.Conn().QueryRow("SELECT prompt_tokens, completion_tokens, total_tokens FROM audit_logs ORDER BY id LIMIT 1").Scan(&prompt, &completion, &total); err != nil {
				t.Fatalf("scan: %v", err)
			}
			if prompt != 0 || completion != 0 || total != 0 {
				t.Errorf("tokens = %d/%d/%d, want 0/0/0 (no usage)", prompt, completion, total)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("audit row not persisted within timeout")
}

func TestNewChatHandler_InvalidUpstreamURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{"valid url", "https://api.deepseek.com/v1", false},
		{"valid http url", "http://localhost:11434/v1", false},
		{"invalid scheme", "ht!tp://example.com", true},
		{"missing scheme", "://missing-scheme", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := &config.Config{
				Upstream: config.UpstreamConfig{URL: tt.url, Key: "test-key"},
			}
			database := &db.DB{}

			_, err := NewChatHandler(cfg, database)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("NewChatHandler: expected error for url %q, got nil", tt.url)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewChatHandler: unexpected error: %v", err)
			}
		})
	}
}

func TestNewChatHandler_HandlerBadJSON(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		Upstream: config.UpstreamConfig{URL: "https://api.example.com/v1", Key: "test-key"},
	}
	database, err := db.Init(t.TempDir() + "/audit.db")
	if err != nil {
		t.Fatalf("db.Init: %v", err)
	}
	defer database.Close()

	handler, err := NewChatHandler(cfg, database)
	if err != nil {
		t.Fatalf("NewChatHandler: %v", err)
	}

	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantSubstr string
	}{
		{
			name:       "invalid json body",
			body:       `{invalid json`,
			wantStatus: http.StatusBadRequest,
			wantSubstr: "Invalid request JSON payload",
		},
		{
			name:       "empty body",
			body:       ``,
			wantStatus: http.StatusBadRequest,
			wantSubstr: "Invalid request JSON payload",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(tt.body))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if !bytes.Contains(rec.Body.Bytes(), []byte(tt.wantSubstr)) {
				t.Errorf("body = %q, want substring %q", rec.Body.String(), tt.wantSubstr)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
		})
	}
}

func TestNewChatHandler_ProxiesToUpstream(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("upstream path = %q, want %q", r.URL.Path, "/v1/chat/completions")
		}
		if r.Header.Get("Authorization") != "Bearer test-upstream-key" {
			t.Errorf("upstream Authorization = %q, want %q", r.Header.Get("Authorization"), "Bearer test-upstream-key")
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("upstream Content-Type = %q, want application/json", r.Header.Get("Content-Type"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","model":"test-model","choices":[]}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Upstream: config.UpstreamConfig{URL: upstream.URL + "/v1", Key: "test-upstream-key"},
	}
	database, err := db.Init(t.TempDir() + "/audit.db")
	if err != nil {
		t.Fatalf("db.Init: %v", err)
	}
	defer database.Close()

	handler, err := NewChatHandler(cfg, database)
	if err != nil {
		t.Fatalf("NewChatHandler: %v", err)
	}

	body := `{"model":"test-model","messages":[{"role":"user","content":"hi"}],"stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req = req.WithContext(context.Background())
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("chatcmpl-1")) {
		t.Errorf("body = %q, want substring %q", rec.Body.String(), "chatcmpl-1")
	}
}
