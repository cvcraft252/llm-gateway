package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/cvcraft252/llm-gateway/internal/config"
	"github.com/cvcraft252/llm-gateway/internal/db"
)

type contextKey string

const (
	ctxKeyStart contextKey = "start"
	ctxKeyModel contextKey = "model"
)

type chatRequest struct {
	Model string `json:"model"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type streamChunk struct {
	Usage *Usage `json:"usage"`
}

type nonStreamResponse struct {
	Usage *Usage `json:"usage"`
}

// byteBufferPool implements httputil.BufferPool using sync.Pool for zero-allocation proxying.
type byteBufferPool struct {
	pool sync.Pool
}

func newByteBufferPool() *byteBufferPool {
	return &byteBufferPool{
		pool: sync.Pool{
			New: func() any {
				b := make([]byte, 32*1024) // Standard 32KB buffer size
				return &b
			},
		},
	}
}

func (p *byteBufferPool) Get() []byte {
	return *(p.pool.Get().(*[]byte))
}

func (p *byteBufferPool) Put(b []byte) {
	p.pool.Put(&b)
}

// Custom transport configured for high concurrency connection reuse.
var proxyTransport = &http.Transport{
	MaxIdleConns:        100,
	MaxIdleConnsPerHost: 100, // Crucial for resolving connection reuse bottlenecks
	IdleConnTimeout:     90 * time.Second,
}

// observedBody intercepts the read stream to extract token usage without adding latency.
type observedBody struct {
	io.ReadCloser
	start      time.Time
	model      string
	isStream   bool
	database   *db.DB
	statusCode int
	lineBuf    []byte
	finalUsage *Usage
}

func (o *observedBody) Read(p []byte) (n int, err error) {
	n, err = o.ReadCloser.Read(p)
	if n > 0 {
		o.processBytes(p[:n])
	}
	return n, err
}

func (o *observedBody) processBytes(b []byte) {
	o.lineBuf = append(o.lineBuf, b...)
	for {
		idx := bytes.IndexByte(o.lineBuf, '\n')
		if idx == -1 {
			break
		}
		line := o.lineBuf[:idx+1]
		o.parseLine(line)
		o.lineBuf = o.lineBuf[idx+1:]
	}
}

func (o *observedBody) parseLine(line []byte) {
	trimmed := bytes.TrimSpace(line)
	if dataPayload, found := bytes.CutPrefix(trimmed, []byte("data: ")); found {
		if !bytes.Equal(dataPayload, []byte("[DONE]")) {
			var chunk streamChunk
			if err := json.Unmarshal(dataPayload, &chunk); err == nil {
				if chunk.Usage != nil {
					o.finalUsage = chunk.Usage
				}
			}
		}
	}
}

func (o *observedBody) Close() error {
	err := o.ReadCloser.Close()

	duration := time.Since(o.start)
	audit := &db.AuditLog{
		Model:      o.model,
		StatusCode: o.statusCode,
		IsStream:   o.isStream,
		DurationMs: duration.Milliseconds(),
	}

	if o.finalUsage != nil {
		audit.PromptTokens = o.finalUsage.PromptTokens
		audit.CompletionTokens = o.finalUsage.CompletionTokens
		audit.TotalTokens = o.finalUsage.TotalTokens

		slog.Info("Request completed",
			"status", o.statusCode,
			"is_stream", o.isStream,
			"duration_ms", duration.Milliseconds(),
			"prompt_tokens", o.finalUsage.PromptTokens,
			"completion_tokens", o.finalUsage.CompletionTokens,
			"total_tokens", o.finalUsage.TotalTokens,
		)
	} else {
		slog.Info("Request completed (No Usage)",
			"status", o.statusCode,
			"is_stream", o.isStream,
			"duration_ms", duration.Milliseconds(),
		)
	}

	o.database.InsertAsync(audit)
	return err
}

func NewChatHandler(cfg *config.Config, database *db.DB) http.HandlerFunc {
	targetURL, _ := url.Parse(cfg.Upstream.URL)
	bufPool := newByteBufferPool()

	// Initialize the industry-standard reverse proxy
	proxy := &httputil.ReverseProxy{
		Transport:     proxyTransport,
		FlushInterval: -1, // Force immediate flushing to preserve SSE typing effect
		BufferPool:    bufPool,
		Director: func(req *http.Request) {
			req.URL.Scheme = targetURL.Scheme
			req.URL.Host = targetURL.Host
			req.URL.Path = targetURL.Path + "/chat/completions"
			req.Host = targetURL.Host

			req.Header.Set("Authorization", "Bearer "+cfg.Upstream.Key)
			req.Header.Set("Content-Type", "application/json")
		},
		ModifyResponse: func(resp *http.Response) error {
			ctx := resp.Request.Context()
			start, _ := ctx.Value(ctxKeyStart).(time.Time)
			model, _ := ctx.Value(ctxKeyModel).(string)

			contentType := resp.Header.Get("Content-Type")
			isStream := strings.Contains(strings.ToLower(contentType), "text/event-stream")

			// Intercept stream without payload decoding overhead
			resp.Body = &observedBody{
				ReadCloser: resp.Body,
				start:      start,
				model:      model,
				isStream:   isStream,
				database:   database,
				statusCode: resp.StatusCode,
			}
			return nil
		},
	}

	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		slog.Info("Received chat request", "path", r.URL.Path, "method", r.Method)

		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			slog.Error("Failed to read body", "error", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		r.Body.Close()

		var reqObj chatRequest
		_ = json.Unmarshal(bodyBytes, &reqObj)

		r.Body = io.NopCloser(bytes.NewReader(bodyBytes))

		// Safely inject metadata into request context across concurrent goroutines
		ctx := r.Context()
		ctx = context.WithValue(ctx, ctxKeyStart, start)
		ctx = context.WithValue(ctx, ctxKeyModel, reqObj.Model)
		r = r.WithContext(ctx)

		proxy.ServeHTTP(w, r)
	}
}