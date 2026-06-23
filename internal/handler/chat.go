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

var proxyTransport = &http.Transport{
	MaxIdleConns:        100,
	MaxIdleConnsPerHost: 100,
	IdleConnTimeout:     90 * time.Second,
}

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

	// Value Copy here is vital for safe DB InsertAsync
	audit := db.AuditLog{
		Upstream:   o.upstream,
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

	// Pass pointer to fresh copy
	o.database.InsertAsync(&audit)
	return err
}

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

		maxRetries := rtr.MaxRetries()
		attempt := 0
		retryBackoff := rtr.RetryBackoff()

		for {
			// Skip degraded upstreams if fallback alternates exist
			for up != nil && rtr.IsDegraded(up.Name) {
				if up.Fallback != nil {
					slog.Info("Skipping degraded upstream", "upstream", up.Name, "fallback", up.Fallback.Name)
					up = up.Fallback
					attempt = 0 // Reset attempt counter for new upstream
				} else {
					break
				}
			}

			if up == nil {
				respond.WriteJSONError(w, http.StatusBadGateway, "No available upstreams")
				return
			}

			targetModel := reqObj.Model
			if alias, ok := up.Aliases[reqObj.Model]; ok {
				targetModel = alias
			}

			outBody := bodyBytes
			if targetModel != reqObj.Model {
				outBody, err = rewriteModel(bodyBytes, targetModel)
				if err != nil {
					slog.Error("Failed to rewrite model alias", "error", err)
					respond.WriteJSONError(w, http.StatusInternalServerError, "failed to rewrite model alias")
					return
				}
			}

			ctx, cancel := context.WithTimeout(r.Context(), rtr.RequestTimeout())

			ctx = context.WithValue(ctx, ctxKeyStart, start)
			ctx = context.WithValue(ctx, ctxKeyModel, reqObj.Model)
			ctx = context.WithValue(ctx, ctxKeyUpstream, up)

			// Always create a fresh clone of request for reverse proxy
			proxyReq := r.Clone(ctx)
			proxyReq.Body = io.NopCloser(bytes.NewReader(outBody))
			proxyReq.ContentLength = int64(len(outBody))

			slog.Info("Routing request",
				"model", reqObj.Model,
				"target_model", targetModel,
				"upstream", up.Name,
				"attempt", attempt,
			)

			// Setup reverse proxy for this attempt.
			var proxyErr error

			currentUp := up

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
						return fmt.Errorf("upstream returned 5xx status: %d", resp.StatusCode)
					}

					contentType := resp.Header.Get("Content-Type")
					isStream := strings.Contains(strings.ToLower(contentType), "text/event-stream")

					resp.Body = &observedBody{
						ReadCloser: resp.Body,
						start:      start,
						upstream:   currentUp.Name,
						model:      reqObj.Model,
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

			proxy.ServeHTTP(w, proxyReq)
			cancel() // Free this attempt's context

			if proxyErr == nil {
				rtr.RecordSuccess(currentUp.Name)
				break
			}

			// Record failure on the current upstream
			rtr.RecordFailure(currentUp.Name)

			attempt++
			slog.Warn("Upstream request failed", "upstream", currentUp.Name, "error", proxyErr, "attempt", attempt)

			// Determine next hop or abort
			if currentUp.Fallback != nil {
				slog.Info("Failing over to fallback upstream", "from", currentUp.Name, "to", currentUp.Fallback.Name)
				up = currentUp.Fallback
				attempt = 0 // Reset attempt counter for new upstream
			} else if attempt <= maxRetries {
				slog.Info("Retrying same upstream", "upstream", currentUp.Name, "attempt", attempt, "maxRetries", maxRetries)
				if retryBackoff > 0 {
					time.Sleep(retryBackoff)
				}
			} else {
				slog.Error("Max retries reached on upstream and no fallback configured", "upstream", currentUp.Name)
				respond.WriteJSONError(w, http.StatusBadGateway, "Upstream is unavailable")
				return
			}
		}
	}

	return handler, nil
}
