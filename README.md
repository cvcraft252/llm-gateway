# LLM Gateway

A lightweight and high-performance LLM API gateway written in Go. This gateway serves as a reverse proxy that sits between your applications and upstream LLM providers, offering unified routing, authentication, and token observability.

## Features

- **Unified OpenAI Compatibility**: Exposes a standard `/v1/chat/completions` endpoint, allowing seamless integration with existing OpenAI SDKs or clients.
- **Gateway Authorization**: Secures your upstream credentials by requiring a custom gateway key for all incoming requests.
- **Streaming (SSE) Support**: Forwards Server-Sent Events (SSE) chunks to the client in real-time, preserving the typing/streaming experience.
- **Token Usage Extraction**: Intercepts and parses token consumption (`prompt_tokens`, `completion_tokens`, and `total_tokens`) on-the-fly for both streaming and non-streaming responses, outputting structured logs to the console.

## Prerequisites

- Go 1.22 or higher

## Quick Start

### 1. Configuration

Copy the configuration template and create your local `config.yaml`:

```bash
cp config.example.yaml config.yaml
```

Open config.yaml and configure your port, gateway keys, and upstream credentials:

```yaml
server:
  port: 8080
gateway:
  keys:
    - gw-key-123456
upstream:
  url: "https://api.deepseek.com/v1"
  key: "YOUR_REAL_UPSTREAM_API_KEY"
```

### 2. Run the Gateway

Start the gateway server:

```bash
go run cmd/gateway/main.go
```

The server will start listening at :8080 (or your configured port).

### 3. Testing the Endpoints

**Non-Streaming Request**

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

**Streaming Request (SSE)**

```bash
curl -i -X POST http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer gw-key-123456" \
  -d '{
    "model": "deepseek-chat", 
    "messages": [{"role": "user", "content": "Hello!"}],
    "stream": true
  }'
```