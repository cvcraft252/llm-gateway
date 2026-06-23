package handler

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cvcraft252/llm-gateway/internal/config"
	"github.com/cvcraft252/llm-gateway/internal/db"
	"github.com/cvcraft252/llm-gateway/internal/router"

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
		upstream:   "test-upstream",
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
			var upstream, model string
			var status, isStream, prompt, completion, total int
			if err := database.Conn().QueryRow("SELECT upstream, model, status_code, is_stream, prompt_tokens, completion_tokens, total_tokens FROM audit_logs ORDER BY id LIMIT 1").Scan(&upstream, &model, &status, &isStream, &prompt, &completion, &total); err != nil {
				t.Fatalf("scan: %v", err)
			}
			if upstream != "test-upstream" {
				t.Errorf("upstream = %q, want %q", upstream, "test-upstream")
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
		upstream:   "no-usage-upstream",
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
			var upstream string
			var prompt, completion, total int
			if err := database.Conn().QueryRow("SELECT upstream, prompt_tokens, completion_tokens, total_tokens FROM audit_logs ORDER BY id LIMIT 1").Scan(&upstream, &prompt, &completion, &total); err != nil {
				t.Fatalf("scan: %v", err)
			}
			if upstream != "no-usage-upstream" {
				t.Errorf("upstream = %q, want %q", upstream, "no-usage-upstream")
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

func TestRewriteModel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		body        string
		targetModel string
		wantModel   string
		wantErr     bool
	}{
		{
			name:        "rewrite model field",
			body:        `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`,
			targetModel: "gpt-4o",
			wantModel:   "gpt-4o",
		},
		{
			name:        "preserve other fields",
			body:        `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"stream":true,"temperature":0.7}`,
			targetModel: "gpt-4o",
			wantModel:   "gpt-4o",
		},
		{
			name:        "invalid json body",
			body:        `{invalid`,
			targetModel: "gpt-4o",
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			out, err := rewriteModel([]byte(tt.body), tt.targetModel)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("rewriteModel: expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("rewriteModel: %v", err)
			}

			var body map[string]any
			if err := json.Unmarshal(out, &body); err != nil {
				t.Fatalf("output is not valid JSON: %v", err)
			}
			gotModel, ok := body["model"].(string)
			if !ok {
				t.Fatalf("model field missing or not a string in output: %s", out)
			}
			if gotModel != tt.wantModel {
				t.Errorf("model = %q, want %q", gotModel, tt.wantModel)
			}
			if _, ok := body["messages"]; !ok {
				t.Errorf("messages field missing from output: %s", out)
			}
		})
	}
}

func newTestRouter(t *testing.T, upstreams ...config.UpstreamConfig) *router.Router {
	t.Helper()
	cfg := &config.Config{
		Upstreams: upstreams,
		Routing:   config.RoutingConfig{Timeout: 10 * time.Second},
	}
	rtr, err := router.New(cfg)
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	return rtr
}

func TestNewChatHandler_NilRouter(t *testing.T) {
	t.Parallel()

	database, err := db.Init(t.TempDir() + "/audit.db")
	if err != nil {
		t.Fatalf("db.Init: %v", err)
	}
	defer database.Close()

	_, err = NewChatHandler(database, nil)
	if err == nil {
		t.Fatalf("NewChatHandler: expected error for nil router, got nil")
	}
}

func TestNewChatHandler_HandlerBadJSON(t *testing.T) {
	t.Parallel()

	rtr := newTestRouter(t, config.UpstreamConfig{
		Name: "test", URL: "https://api.example.com/v1", Key: "test-key",
		Models: []string{"test-model"},
	})
	database, err := db.Init(t.TempDir() + "/audit.db")
	if err != nil {
		t.Fatalf("db.Init: %v", err)
	}
	defer database.Close()

	handler, err := NewChatHandler(database, rtr)
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

func TestNewChatHandler_ModelNotFound(t *testing.T) {
	t.Parallel()

	rtr := newTestRouter(t, config.UpstreamConfig{
		Name: "test", URL: "https://api.example.com/v1", Key: "test-key",
		Models: []string{"known-model"},
	})
	database, err := db.Init(t.TempDir() + "/audit.db")
	if err != nil {
		t.Fatalf("db.Init: %v", err)
	}
	defer database.Close()

	handler, err := NewChatHandler(database, rtr)
	if err != nil {
		t.Fatalf("NewChatHandler: %v", err)
	}

	body := `{"model":"unknown-model","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("model not found")) {
		t.Errorf("body = %q, want substring %q", rec.Body.String(), "model not found")
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

func TestNewChatHandler_ProxiesToCorrectUpstream(t *testing.T) {
	t.Parallel()

	var deepseekCalled, openaiCalled bool
	var deepseekMu, openaiMu sync.Mutex

	deepseek := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deepseekMu.Lock()
		deepseekCalled = true
		deepseekMu.Unlock()

		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("deepseek path = %q, want /v1/chat/completions", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer sk-deepseek" {
			t.Errorf("deepseek Authorization = %q, want Bearer sk-deepseek", r.Header.Get("Authorization"))
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["model"] != "deepseek-chat" {
			t.Errorf("deepseek received model = %v, want deepseek-chat", body["model"])
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"ds-1","model":"deepseek-chat"}`))
	}))
	defer deepseek.Close()

	openai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		openaiMu.Lock()
		openaiCalled = true
		openaiMu.Unlock()

		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("openai path = %q, want /v1/chat/completions", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer sk-openai" {
			t.Errorf("openai Authorization = %q, want Bearer sk-openai", r.Header.Get("Authorization"))
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["model"] != "gpt-4o" {
			t.Errorf("openai received model = %v, want gpt-4o (alias should be rewritten)", body["model"])
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"oai-1","model":"gpt-4o"}`))
	}))
	defer openai.Close()

	rtr := newTestRouter(t,
		config.UpstreamConfig{
			Name: "deepseek", URL: deepseek.URL + "/v1", Key: "sk-deepseek",
			Models: []string{"deepseek-chat"},
		},
		config.UpstreamConfig{
			Name: "openai", URL: openai.URL + "/v1", Key: "sk-openai",
			Models:  []string{"gpt-4o"},
			Aliases: map[string]string{"gpt-4": "gpt-4o"},
		},
	)
	database, err := db.Init(t.TempDir() + "/audit.db")
	if err != nil {
		t.Fatalf("db.Init: %v", err)
	}
	defer database.Close()

	handler, err := NewChatHandler(database, rtr)
	if err != nil {
		t.Fatalf("NewChatHandler: %v", err)
	}

	t.Run("routes deepseek-chat to deepseek upstream", func(t *testing.T) {
		body := `{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", rec.Code)
		}
		if !bytes.Contains(rec.Body.Bytes(), []byte("ds-1")) {
			t.Errorf("body = %q, want substring ds-1", rec.Body.String())
		}
		deepseekMu.Lock()
		defer deepseekMu.Unlock()
		if !deepseekCalled {
			t.Errorf("deepseek upstream was not called")
		}
	})

	t.Run("routes gpt-4 alias to openai upstream with rewritten model", func(t *testing.T) {
		body := `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", rec.Code)
		}
		if !bytes.Contains(rec.Body.Bytes(), []byte("oai-1")) {
			t.Errorf("body = %q, want substring oai-1", rec.Body.String())
		}
		openaiMu.Lock()
		defer openaiMu.Unlock()
		if !openaiCalled {
			t.Errorf("openai upstream was not called")
		}
	})
}

func TestNewChatHandler_AliasRewritePreservesFields(t *testing.T) {
	t.Parallel()

	var receivedBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&receivedBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"ok"}`))
	}))
	defer upstream.Close()

	rtr := newTestRouter(t, config.UpstreamConfig{
		Name: "test", URL: upstream.URL + "/v1", Key: "sk",
		Models:  []string{"real-model"},
		Aliases: map[string]string{"alias-name": "real-model"},
	})
	database, err := db.Init(t.TempDir() + "/audit.db")
	if err != nil {
		t.Fatalf("db.Init: %v", err)
	}
	defer database.Close()

	handler, err := NewChatHandler(database, rtr)
	if err != nil {
		t.Fatalf("NewChatHandler: %v", err)
	}

	body := `{"model":"alias-name","messages":[{"role":"user","content":"hello"}],"stream":true,"temperature":0.5}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}

	if receivedBody["model"] != "real-model" {
		t.Errorf("upstream received model = %v, want real-model", receivedBody["model"])
	}
	if receivedBody["stream"] != true {
		t.Errorf("stream field = %v, want true", receivedBody["stream"])
	}
	if receivedBody["temperature"] != 0.5 {
		t.Errorf("temperature field = %v, want 0.5", receivedBody["temperature"])
	}
	msgs, ok := receivedBody["messages"].([]any)
	if !ok || len(msgs) != 1 {
		t.Errorf("messages field not preserved, got %v", receivedBody["messages"])
	}
}

func TestNewChatHandler_RetryAndFailover(t *testing.T) {
	t.Parallel()

	// 1. Setup a flaky upstream (fails on first call, succeeds on second)
	var flakyCalls int
	var flakyMu sync.Mutex
	flakyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flakyMu.Lock()
		flakyCalls++
		currentCall := flakyCalls
		flakyMu.Unlock()

		if currentCall == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error": "internal error"}`))
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"flaky-ok","model":"flaky-model"}`))
	}))
	defer flakyServer.Close()

	// 2. Setup a failing upstream (always fails)
	failingServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error": "always fails"}`))
	}))
	defer failingServer.Close()

	// 3. Setup a fallback server (succeeds)
	fallbackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"fallback-ok","model":"fallback-model"}`))
	}))
	defer fallbackServer.Close()

	// 4. Create router with:
	// - Upstream "flaky" with max_retries = 1
	// - Upstream "failing" with fallback = "fallback"
	cfg := &config.Config{
		Upstreams: []config.UpstreamConfig{
			{
				Name:   "flaky",
				URL:    flakyServer.URL + "/v1",
				Key:    "key",
				Models: []string{"flaky-model"},
			},
			{
				Name:     "failing",
				URL:      failingServer.URL + "/v1",
				Key:      "key",
				Models:   []string{"failover-model"},
				Fallback: "fallback",
			},
			{
				Name:   "fallback",
				URL:    fallbackServer.URL + "/v1",
				Key:    "key",
				Models: []string{"fallback-model"},
			},
		},
		Routing: config.RoutingConfig{
			Timeout:      2 * time.Second,
			MaxRetries:   2,
			RetryBackoff: 1 * time.Millisecond,
		},
	}
	rtr, err := router.New(cfg)
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}

	database, err := db.Init(t.TempDir() + "/audit.db")
	if err != nil {
		t.Fatalf("db.Init: %v", err)
	}
	defer database.Close()

	handler, err := NewChatHandler(database, rtr)
	if err != nil {
		t.Fatalf("NewChatHandler: %v", err)
	}

	t.Run("retry-then-success path", func(t *testing.T) {
		body := `{"model":"flaky-model","messages":[]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("expected 200, got %d. Body: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "flaky-ok") {
			t.Errorf("expected body to contain 'flaky-ok', got: %s", rec.Body.String())
		}
		flakyMu.Lock()
		calls := flakyCalls
		flakyMu.Unlock()
		if calls != 2 {
			t.Errorf("expected 2 calls to flaky server, got %d", calls)
		}
	})

	t.Run("retry-then-fallback path", func(t *testing.T) {
		body := `{"model":"failover-model","messages":[]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("expected 200, got %d. Body: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "fallback-ok") {
			t.Errorf("expected body to contain 'fallback-ok', got: %s", rec.Body.String())
		}
	})
}

func TestNewChatHandler_RetryExhausted(t *testing.T) {
	t.Parallel()

	var callCount int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		callCount++
		mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfg := &config.Config{
		Upstreams: []config.UpstreamConfig{
			{
				Name:   "bad",
				URL:    srv.URL + "/v1",
				Key:    "key",
				Models: []string{"bad-model"},
			},
		},
		Routing: config.RoutingConfig{
			Timeout:      2 * time.Second,
			MaxRetries:   2, // 1 primary attempt + 2 retries = 3 total attempts
			RetryBackoff: 1 * time.Millisecond,
		},
	}
	rtr, err := router.New(cfg)
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}

	database, err := db.Init(t.TempDir() + "/audit.db")
	if err != nil {
		t.Fatalf("db.Init: %v", err)
	}
	defer database.Close()

	handler, err := NewChatHandler(database, rtr)
	if err != nil {
		t.Fatalf("NewChatHandler: %v", err)
	}

	body := `{"model":"bad-model","messages":[]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("expected 502, got %d. Body: %s", rec.Code, rec.Body.String())
	}

	mu.Lock()
	calls := callCount
	mu.Unlock()
	if calls != 3 {
		t.Errorf("expected 3 total attempts (1 try + 2 retries), got %d", calls)
	}
}

func TestNewChatHandler_DegradedSkipping(t *testing.T) {
	t.Parallel()

	var primaryCalls int
	var primaryMu sync.Mutex
	primaryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryMu.Lock()
		primaryCalls++
		primaryMu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer primaryServer.Close()

	fallbackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"fallback-ok","model":"fallback-model"}`))
	}))
	defer fallbackServer.Close()

	cfg := &config.Config{
		Upstreams: []config.UpstreamConfig{
			{
				Name:     "primary",
				URL:      primaryServer.URL + "/v1",
				Key:      "key",
				Models:   []string{"some-model"},
				Fallback: "fallback",
			},
			{
				Name:   "fallback",
				URL:    fallbackServer.URL + "/v1",
				Key:    "key",
				Models: []string{"fallback-model"},
			},
		},
		Routing: config.RoutingConfig{
			Timeout:           2 * time.Second,
			MaxRetries:        0,
			HealthMaxFailures: 1, // Degrades after 1 failure
			HealthCooldown:    10 * time.Second,
		},
	}
	rtr, err := router.New(cfg)
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}

	database, err := db.Init(t.TempDir() + "/audit.db")
	if err != nil {
		t.Fatalf("db.Init: %v", err)
	}
	defer database.Close()

	handler, err := NewChatHandler(database, rtr)
	if err != nil {
		t.Fatalf("NewChatHandler: %v", err)
	}

	body := `{"model":"some-model","messages":[]}`

	// First Request: goes to primary, fails, triggers fallback failover.
	// This record a failure on "primary", marking it degraded (max_failures is 1)
	req1 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("Req1 failed: %d - %s", rec1.Code, rec1.Body.String())
	}

	primaryMu.Lock()
	calls1 := primaryCalls
	primaryMu.Unlock()
	if calls1 != 1 {
		t.Errorf("expected 1 call to primary server so far, got %d", calls1)
	}

	// Second Request: "primary" is degraded. Handler should skip "primary" entirely
	// and route straight to "fallback".
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("Req2 failed: %d - %s", rec2.Code, rec2.Body.String())
	}

	primaryMu.Lock()
	calls2 := primaryCalls
	primaryMu.Unlock()
	if calls2 != 1 {
		t.Errorf("expected primary server to be skipped (calls should remain 1), got %d", calls2)
	}
}

func TestNewChatHandler_StreamResponseDoesNotRetry(t *testing.T) {
	t.Parallel()

	var callCount int
	var mu sync.Mutex

	streamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		callCount++
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer streamServer.Close()

	fallbackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("fallback server should not be called when primary returned 200")
	}))
	defer fallbackServer.Close()

	cfg := &config.Config{
		Upstreams: []config.UpstreamConfig{
			{
				Name:     "stream",
				URL:      streamServer.URL + "/v1",
				Key:      "key",
				Models:   []string{"stream-model"},
				Fallback: "fallback",
			},
			{
				Name:   "fallback",
				URL:    fallbackServer.URL + "/v1",
				Key:    "key",
				Models: []string{"fallback-model"},
			},
		},
		Routing: config.RoutingConfig{
			Timeout:      2 * time.Second,
			MaxRetries:   3,
			RetryBackoff: 1 * time.Millisecond,
		},
	}
	rtr, err := router.New(cfg)
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}

	database, err := db.Init(t.TempDir() + "/audit.db")
	if err != nil {
		t.Fatalf("db.Init: %v", err)
	}
	defer database.Close()

	handler, err := NewChatHandler(database, rtr)
	if err != nil {
		t.Fatalf("NewChatHandler: %v", err)
	}

	body := `{"model":"stream-model","messages":[]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}

	mu.Lock()
	calls := callCount
	mu.Unlock()
	if calls != 1 {
		t.Errorf("stream upstream should be called exactly once (no retry after 200), got %d", calls)
	}
}
