package handler

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/cvcraft252/llm-gateway/internal/config"
	"github.com/cvcraft252/llm-gateway/internal/db"
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

func NewChatHandler(cfg *config.Config, database *db.DB) http.HandlerFunc {
	client := &http.Client{
		Timeout: 5 * time.Minute,
	}

	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		slog.Info("Received chat request", "path", r.URL.Path, "method", r.Method)

		// Read and cache body to extract model name
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			slog.Error("Failed to read body", "error", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		r.Body.Close()

		var reqObj chatRequest
		_ = json.Unmarshal(bodyBytes, &reqObj)

		// Restore r.Body for upstream forwarding
		r.Body = io.NopCloser(bytes.NewReader(bodyBytes))

		upstreamURL := cfg.Upstream.URL + "/chat/completions"
		req, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL, r.Body)
		if err != nil {
			slog.Error("Failed to create upstream request", "error", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"error": "Internal Gateway Error"}`))
			return
		}

		for k, vv := range r.Header {
			if strings.ToLower(k) == "authorization" {
				continue
			}
			for _, v := range vv {
				req.Header.Add(k, v)
			}
		}
		req.Header.Set("Authorization", "Bearer "+cfg.Upstream.Key)

		resp, err := client.Do(req)
		if err != nil {
			slog.Error("Failed to call upstream provider", "error", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			w.Write([]byte(`{"error": "Bad Gateway"}`))
			return
		}
		defer resp.Body.Close()

		for k, vv := range resp.Header {
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)

		contentType := resp.Header.Get("Content-Type")
		isStream := strings.Contains(strings.ToLower(contentType), "text/event-stream")

		var finalUsage *Usage

		if isStream {
			flusher, ok := w.(http.Flusher)
			if !ok {
				slog.Warn("ResponseWriter does not support Flusher, buffering instead")
				io.Copy(w, resp.Body)
			} else {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Cache-Control", "no-cache")
				w.Header().Set("Connection", "keep-alive")

				reader := bufio.NewReader(resp.Body)
				for {
					line, err := reader.ReadBytes('\n')
					if len(line) > 0 {
						_, writeErr := w.Write(line)
						if writeErr != nil {
							slog.Warn("Client disconnected during stream")
							return
						}
						flusher.Flush()

						trimmed := bytes.TrimSpace(line)
						if dataPayload, found := bytes.CutPrefix(trimmed, []byte("data: ")); found {
							if !bytes.Equal(dataPayload, []byte("[DONE]")) {
								var chunk streamChunk
								if err := json.Unmarshal(dataPayload, &chunk); err == nil {
									if chunk.Usage != nil {
										finalUsage = chunk.Usage
									}
								}
							}
						}
					}
					if err != nil {
						if err == io.EOF {
							break
						}
						slog.Error("Error reading upstream stream", "error", err)
						return
					}
				}
			}
		} else {
			bodyBytes, err := io.ReadAll(resp.Body)
			if err != nil {
				slog.Error("Failed to read upstream response body", "error", err)
				return
			}

			var nonStreamResp nonStreamResponse
			if err := json.Unmarshal(bodyBytes, &nonStreamResp); err == nil {
				finalUsage = nonStreamResp.Usage
			}

			_, err = w.Write(bodyBytes)
			if err != nil {
				slog.Error("Failed to write body to client", "error", err)
				return
			}
		}

		duration := time.Since(start)

		// Prepare and submit log to SQLite asynchronously
		audit := &db.AuditLog{
			Model:      reqObj.Model,
			StatusCode: resp.StatusCode,
			IsStream:   isStream,
			DurationMs: duration.Milliseconds(),
		}
		if finalUsage != nil {
			audit.PromptTokens = finalUsage.PromptTokens
			audit.CompletionTokens = finalUsage.CompletionTokens
			audit.TotalTokens = finalUsage.TotalTokens

			slog.Info("Request completed",
				"status", resp.StatusCode,
				"is_stream", isStream,
				"duration_ms", duration.Milliseconds(),
				"prompt_tokens", finalUsage.PromptTokens,
				"completion_tokens", finalUsage.CompletionTokens,
				"total_tokens", finalUsage.TotalTokens,
			)
		} else {
			slog.Info("Request completed (No Usage)",
				"status", resp.StatusCode,
				"is_stream", isStream,
				"duration_ms", duration.Milliseconds(),
			)
		}

		database.InsertAsync(audit)
	}
}
