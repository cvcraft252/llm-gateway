# LLM Gateway

A lightweight and high-performance LLM API gateway written in Go. It sits between your applications and upstream LLM providers, routing requests by model to the matching provider while providing unified OpenAI compatibility, gateway authorization, SSE streaming, and token-usage auditing.

## Features

- **Multi-Provider Routing**: Route requests to different upstream providers (OpenAI, DeepSeek, Mistral, Ollama, vLLM, Groq, ...) based on the `model` field — all through a single `/v1/chat/completions` endpoint.
- **Model Aliases**: Map familiar model names (e.g. `gpt-4`) to the actual model served by the upstream (e.g. `gpt-4o`), so clients don't need to track provider-specific names.
- **Unified OpenAI Compatibility**: Any OpenAI-compatible SDK can talk to the gateway; non-OpenAI providers that expose an OpenAI-compatible endpoint (Ollama, vLLM, ...) work out of the box.
- **Gateway Authorization**: Secure upstream credentials behind custom gateway keys (Bearer auth).
- **Streaming (SSE) Support**: Forwards Server-Sent Events chunks to the client in real-time with immediate flushing, preserving the typing/streaming experience.
- **Token Usage Extraction**: Intercepts and parses token consumption on-the-fly for both streaming and non-streaming responses, persisting it to a local SQLite audit log.
- **Per-Request Timeout**: Configurable deadline per request so a slow upstream cannot stall a client indefinitely.

## Prerequisites

- Go 1.26 or higher (the `go.mod` pins `go 1.26.4`)

## Quick Start

### 1. Configuration

Copy the configuration template and create your local `config.yaml`:

```bash
cp config.example.yaml config.yaml
```

Open `config.yaml` and configure your port, gateway keys, and one or more upstreams:

```yaml
server:
  port: 8080
gateway:
  keys:
    - gw-key-123456
upstreams:
  - name: deepseek
    url: "https://api.deepseek.com/v1"
    key: "YOUR_DEEPSEEK_API_KEY"
    models:
      - deepseek-chat
      - deepseek-reasoner
  - name: openai
    url: "https://api.openai.com/v1"
    key: "YOUR_OPENAI_API_KEY"
    models:
      - gpt-4o
      - gpt-4o-mini
    aliases:
      gpt-4: gpt-4o
routing:
  timeout: 120s
```

> **Note**: `upstream.url` must include the `/v1` base path. The gateway appends `/chat/completions` to it.

### 2. Run the Gateway

```bash
go run cmd/gateway/main.go
```

The server will start listening at `:8080` (or your configured port).

### 3. Send Requests

**Non-Streaming**

```bash
curl -i -X POST http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer gw-key-123456" \
  -d '{
    "model": "deepseek-chat",
    "messages": [{"role": "user", "content": "Introduce Go language in one sentence"}],
    "stream": false
  }'
```

**Streaming (SSE)**

```bash
curl -i -X POST http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer gw-key-123456" \
  -d '{
    "model": "gpt-4o",
    "messages": [{"role": "user", "content": "Introduce Go language in one sentence"}],
    "stream": true
  }'
```

**Using an alias**

```bash
curl -i -X POST http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer gw-key-123456" \
  -d '{
    "model": "gpt-4",
    "messages": [{"role": "user", "content": "Hello"}]
  }'
```

The gateway rewrites `gpt-4` to `gpt-4o` before forwarding to the OpenAI upstream.

### 4. Health Check

```bash
curl http://localhost:8080/health
# {"status": "ok"}
```

## Configuration Reference

| Field | Type | Description |
|---|---|---|
| `server.port` | int | Port the gateway listens on. |
| `gateway.keys` | []string | Bearer tokens accepted by the auth middleware. |
| `upstreams[]` | list | One or more upstream providers. |
| `upstreams[].name` | string | Unique upstream identifier (used in audit logs). |
| `upstreams[].url` | string | Base URL including `/v1` (e.g. `https://api.openai.com/v1`). |
| `upstreams[].key` | string | Provider API key. Empty for no-auth providers like Ollama. |
| `upstreams[].models` | []string | Models served by this upstream. Each model must be unique across all upstreams. |
| `upstreams[].aliases` | map | Optional alias → real-model mapping. The client sends the alias; the gateway rewrites it before forwarding. |
| `routing.timeout` | duration | Per-request timeout. Defaults to `120s` if omitted or ≤ 0. |

## Migration from single-upstream config

If you have an older `config.yaml` with a single `upstream:` block, rename it to `upstreams:` and wrap the entry in a list with `name`, `models`, and optional `aliases`. See `config.example.yaml` for a working example.

## Audit Log

Token usage for every proxied request is persisted asynchronously to a local SQLite database (`gateway.db`). Each row records the upstream name, model, HTTP status, stream flag, duration, and token counts. The database uses WAL mode with a 5-second busy timeout to handle concurrent writes.
