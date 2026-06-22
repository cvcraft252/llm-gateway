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

	"github.com/cvcraft252/llm-gateway/internal/config"
	"github.com/cvcraft252/llm-gateway/internal/db"
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

// observedBody intercepts the read stream to extract token usage without adding latency.
type observedBody struct {
	io.ReadCloser
	start      time.Time
	upstream   string
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
		Upstream:   o.upstream,
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
			"upstream", o.upstream,
			"status", o.statusCode,
			"is_stream", o.isStream,
			"duration_ms", duration.Milliseconds(),
			"prompt_tokens", o.finalUsage.PromptTokens,
			"completion_tokens", o.finalUsage.CompletionTokens,
			"total_tokens", o.finalUsage.TotalTokens,
		)
	} else {
		slog.Info("Request completed (No Usage)",
			"upstream", o.upstream,
			"status", o.statusCode,
			"is_stream", o.isStream,
			"duration_ms", duration.Milliseconds(),
		)
	}

	o.database.InsertAsync(audit)
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

func NewChatHandler(_ *config.Config, database *db.DB, rtr *router.Router) (http.HandlerFunc, error) {
	if rtr == nil {
		return nil, errors.New("router is nil")
	}
	bufPool := newByteBufferPool()

	proxy := &httputil.ReverseProxy{
		Transport:     proxyTransport,
		FlushInterval: -1,
		BufferPool:    bufPool,
		Director: func(req *http.Request) {
			ctx := req.Context()
			up, _ := ctx.Value(ctxKeyUpstream).(*router.Upstream)

			req.URL.Scheme = up.URL.Scheme
			req.URL.Host = up.URL.Host
			req.URL.Path = up.URL.Path + "/chat/completions"
			req.Host = up.URL.Host

			req.Header.Set("Authorization", "Bearer "+up.Key)
			req.Header.Set("Content-Type", "application/json")
		},
		ModifyResponse: func(resp *http.Response) error {
			ctx := resp.Request.Context()
			start, _ := ctx.Value(ctxKeyStart).(time.Time)
			model, _ := ctx.Value(ctxKeyModel).(string)
			up, _ := ctx.Value(ctxKeyUpstream).(*router.Upstream)

			contentType := resp.Header.Get("Content-Type")
			isStream := strings.Contains(strings.ToLower(contentType), "text/event-stream")

			upstreamName := ""
			if up != nil {
				upstreamName = up.Name
			}

			resp.Body = &observedBody{
				ReadCloser: resp.Body,
				start:      start,
				upstream:   upstreamName,
				model:      model,
				isStream:   isStream,
				database:   database,
				statusCode: resp.StatusCode,
			}
			return nil
		},
	}

	handler := func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		slog.Info("Received chat request", "path", r.URL.Path, "method", r.Method)

		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			slog.Error("Failed to read body", "error", err)
			writeJSONError(w, http.StatusBadRequest, "Failed to read request body")
			return
		}
		_ = r.Body.Close()

		var reqObj chatRequest
		if err := json.Unmarshal(bodyBytes, &reqObj); err != nil {
			slog.Warn("Failed to decode client JSON", "error", err, "ip", r.RemoteAddr)
			writeJSONError(w, http.StatusBadRequest, "Invalid request JSON payload")
			return
		}

		up, targetModel, err := rtr.Pick(reqObj.Model)
		if err != nil {
			slog.Warn("Model not routed", "model", reqObj.Model, "ip", r.RemoteAddr, "error", err)
			writeJSONError(w, http.StatusNotFound, fmt.Sprintf("model not found: %s", reqObj.Model))
			return
		}

		outBody := bodyBytes
		if targetModel != reqObj.Model {
			outBody, err = rewriteModel(bodyBytes, targetModel)
			if err != nil {
				slog.Error("Failed to rewrite model alias", "error", err)
				writeJSONError(w, http.StatusInternalServerError, "failed to rewrite model alias")
				return
			}
		}
		r.Body = io.NopCloser(bytes.NewReader(outBody))
		r.ContentLength = int64(len(outBody))

		timeout := rtr.RequestTimeout()
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()

		ctx = context.WithValue(ctx, ctxKeyStart, start)
		ctx = context.WithValue(ctx, ctxKeyModel, reqObj.Model)
		ctx = context.WithValue(ctx, ctxKeyUpstream, up)
		r = r.WithContext(ctx)

		slog.Info("Routing request",
			"model", reqObj.Model,
			"target_model", targetModel,
			"upstream", up.Name,
		)

		proxy.ServeHTTP(w, r)
	}

	return handler, nil
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"error": %q}`, message)
}
