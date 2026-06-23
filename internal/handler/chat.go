package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"
	"time"

	"github.com/cvcraft252/llm-gateway/internal/db"
	"github.com/cvcraft252/llm-gateway/internal/middleware"
	"github.com/cvcraft252/llm-gateway/internal/respond"
	"github.com/cvcraft252/llm-gateway/internal/router"
)

type contextKey string

const (
	ctxKeyStart    contextKey = "start"
	ctxKeyModel    contextKey = "model"
	ctxKeyUpstream contextKey = "upstream"
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

// byteBufferPool implements httputil.BufferPool using sync.Pool for zero-allocation proxying.
type byteBufferPool struct {
	pool sync.Pool
}

func newByteBufferPool() *byteBufferPool {
	return &byteBufferPool{
		pool: sync.Pool{
			New: func() any {
				b := make([]byte, 32*1024)
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

// proxyTransport is shared across all proxied requests. Per-host idle limits
// are intentionally high because the gateway may fan out to multiple upstreams.
var proxyTransport = &http.Transport{
	MaxIdleConns:        100,
	MaxIdleConnsPerHost: 100,
	IdleConnTimeout:     90 * time.Second,
}

// retryableWriter wraps http.ResponseWriter to track whether any bytes have
// been written to the client. Once headers or a body are sent, the response is
// committed and retrying against another upstream would corrupt the HTTP
// response (duplicate status line + interleaved body). The retry loop checks
// headerWritten to decide whether a retry is still safe.
type retryableWriter struct {
	http.ResponseWriter
	headerWritten bool
}

func (rw *retryableWriter) WriteHeader(code int) {
	rw.headerWritten = true
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *retryableWriter) Write(b []byte) (int, error) {
	rw.headerWritten = true
	return rw.ResponseWriter.Write(b)
}

// observedBody intercepts the read stream to extract token usage without adding latency.
type observedBody struct {
	io.ReadCloser
	start      time.Time
	upstream   string
	keyID      string
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
			// Malformed chunks in a live SSE stream are expected (partial
			// frames, provider-specific keepalives). Silently skipping them
			// keeps the proxy path fast and avoids log noise; usage extraction
			// only needs the final well-formed chunk.
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
	audit := db.AuditLog{
		Upstream:   o.upstream,
		KeyID:      o.keyID,
		Model:      o.model,
		StatusCode: o.statusCode,
		IsStream:   o.isStream,
		DurationMs: duration.Milliseconds(),
	}

	msg := "Request completed (No Usage)"
	logArgs := []any{
		"upstream", o.upstream,
		"status", o.statusCode,
		"is_stream", o.isStream,
		"duration_ms", duration.Milliseconds(),
	}

	if o.finalUsage != nil {
		audit.PromptTokens = o.finalUsage.PromptTokens
		audit.CompletionTokens = o.finalUsage.CompletionTokens
		audit.TotalTokens = o.finalUsage.TotalTokens

		msg = "Request completed"
		logArgs = append(logArgs,
			"prompt_tokens", o.finalUsage.PromptTokens,
			"completion_tokens", o.finalUsage.CompletionTokens,
			"total_tokens", o.finalUsage.TotalTokens,
		)
	}

	slog.Info(msg, logArgs...)

	// InsertAsync copies the struct by value internally, so passing the
	// address of a local is safe — the goroutine receives its own copy.
	o.database.InsertAsync(&audit)
	return err
}

// rewriteModel replaces the model field in the JSON body when an alias maps
// to a different target model. Returns the original bytes when no rewrite is
// needed. Uses map[string]any to preserve all other fields from the client.
func rewriteModel(bodyBytes []byte, targetModel string) ([]byte, error) {
	var body map[string]any
	if err := json.Unmarshal(bodyBytes, &body); err != nil {
		return nil, fmt.Errorf("rewrite model: %w", err)
	}
	body["model"] = targetModel
	return json.Marshal(body)
}

func NewChatHandler(database *db.DB, rtr *router.Router) (http.HandlerFunc, error) {
	if rtr == nil {
		return nil, errors.New("router is nil")
	}
	bufPool := newByteBufferPool()

	// A single ReverseProxy is reused across all retry attempts. The Director
	// and ModifyResponse closures read currentUp and proxyErr via pointer
	// indirection so each attempt targets the correct upstream.
	var currentUp *router.Upstream
	var proxyErr error

	proxy := &httputil.ReverseProxy{
		Transport:     proxyTransport,
		FlushInterval: -1,
		BufferPool:    bufPool,
		Director: func(req *http.Request) {
			req.URL.Scheme = currentUp.URL.Scheme
			req.URL.Host = currentUp.URL.Host
			req.URL.Path = currentUp.URL.Path + "/chat/completions"
			req.Host = currentUp.URL.Host

			req.Header.Set("Authorization", "Bearer "+currentUp.Key)
			req.Header.Set("Content-Type", "application/json")
		},
		ModifyResponse: func(resp *http.Response) error {
			if resp.StatusCode >= 500 {
				// Drain the body so the keep-alive connection can be reused
				// by the next retry attempt instead of being discarded.
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				return fmt.Errorf("upstream returned 5xx status: %d", resp.StatusCode)
			}

			contentType := resp.Header.Get("Content-Type")
			isStream := strings.Contains(strings.ToLower(contentType), "text/event-stream")

			// Read key_id from the auth middleware via context.
			// YAML bootstrap keys use "yaml" as their key_id.
			keyID, _ := resp.Request.Context().Value(middleware.CtxKeyKeyID).(string)

			resp.Body = &observedBody{
				ReadCloser: resp.Body,
				start:      time.Now(),
				upstream:   currentUp.Name,
				keyID:      keyID,
				model:      resp.Request.Context().Value(ctxKeyModel).(string),
				isStream:   isStream,
				database:   database,
				statusCode: resp.StatusCode,
			}
			return nil
		},
		ErrorHandler: func(rw http.ResponseWriter, req *http.Request, err error) {
			proxyErr = err
		},
	}

	handler := func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		slog.Info("Received chat request", "path", r.URL.Path, "method", r.Method)

		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			slog.Error("Failed to read body", "error", err)
			respond.WriteJSONError(w, http.StatusBadRequest, "Failed to read request body")
			return
		}
		_ = r.Body.Close()

		var reqObj chatRequest
		if err := json.Unmarshal(bodyBytes, &reqObj); err != nil {
			slog.Warn("Failed to decode client JSON", "error", err, "ip", r.RemoteAddr)
			respond.WriteJSONError(w, http.StatusBadRequest, "Invalid request JSON payload")
			return
		}

		up, _, err := rtr.Pick(reqObj.Model)
		if err != nil {
			slog.Warn("Model not routed", "model", reqObj.Model, "ip", r.RemoteAddr, "error", err)
			respond.WriteJSONError(w, http.StatusNotFound, fmt.Sprintf("model not found: %s", reqObj.Model))
			return
		}

		rw := &retryableWriter{ResponseWriter: w}

		maxRetries := rtr.MaxRetries()
		attempt := 0
		retryBackoff := rtr.RetryBackoff()

		for {
			// Skip degraded upstreams if fallback alternates exist.
			for up != nil && rtr.IsDegraded(up.Name) {
				if up.Fallback != nil {
					slog.Info("Skipping degraded upstream", "upstream", up.Name, "fallback", up.Fallback.Name)
					up = up.Fallback
					attempt = 0
				} else {
					break
				}
			}

			if up == nil {
				respond.WriteJSONError(rw, http.StatusBadGateway, "No available upstreams")
				return
			}

			// Re-resolve the alias against the current upstream's alias table.
			// Each fallback upstream may map the same alias to a different
			// real model, so resolution must happen per-upstream, not once
			// at Pick time.
			targetModel := reqObj.Model
			if alias, ok := up.Aliases[reqObj.Model]; ok {
				targetModel = alias
			}

			outBody := bodyBytes
			if targetModel != reqObj.Model {
				outBody, err = rewriteModel(bodyBytes, targetModel)
				if err != nil {
					slog.Error("Failed to rewrite model alias", "error", err)
					respond.WriteJSONError(rw, http.StatusInternalServerError, "failed to rewrite model alias")
					return
				}
			}

			ctx, cancel := context.WithTimeout(r.Context(), rtr.RequestTimeout())

			ctx = context.WithValue(ctx, ctxKeyStart, start)
			ctx = context.WithValue(ctx, ctxKeyModel, reqObj.Model)
			ctx = context.WithValue(ctx, ctxKeyUpstream, up)

			proxyReq := r.Clone(ctx)
			proxyReq.Body = io.NopCloser(bytes.NewReader(outBody))
			proxyReq.ContentLength = int64(len(outBody))

			slog.Info("Routing request",
				"model", reqObj.Model,
				"target_model", targetModel,
				"upstream", up.Name,
				"attempt", attempt,
			)

			currentUp = up
			proxyErr = nil

			proxy.ServeHTTP(rw, proxyReq)
			cancel()

			if proxyErr == nil {
				rtr.RecordSuccess(currentUp.Name)
				break
			}

			// If the response has already started streaming to the client,
			// retrying would corrupt the HTTP response. Abort immediately.
			if rw.headerWritten {
				slog.Error("Upstream failed after response started, cannot retry",
					"upstream", currentUp.Name, "error", proxyErr)
				return
			}

			rtr.RecordFailure(currentUp.Name)

			attempt++
			slog.Warn("Upstream request failed", "upstream", currentUp.Name, "error", proxyErr, "attempt", attempt)

			if currentUp.Fallback != nil {
				slog.Info("Failing over to fallback upstream", "from", currentUp.Name, "to", currentUp.Fallback.Name)
				up = currentUp.Fallback
				attempt = 0
			} else if attempt <= maxRetries {
				slog.Info("Retrying same upstream", "upstream", currentUp.Name, "attempt", attempt, "maxRetries", maxRetries)
				if retryBackoff > 0 {
					select {
					case <-time.After(retryBackoff):
					case <-r.Context().Done():
						return
					}
				}
			} else {
				slog.Error("Max retries reached on upstream and no fallback configured", "upstream", currentUp.Name)
				respond.WriteJSONError(rw, http.StatusBadGateway, "Upstream is unavailable")
				return
			}
		}
	}

	return handler, nil
}
