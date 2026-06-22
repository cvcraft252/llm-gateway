# Roadmap

> Development roadmap for `llm-gateway`, an open-source Go LLM API gateway.
> Reference project for comparison: [theopenco/llmgateway](https://github.com/theopenco/llmgateway) (TypeScript monorepo).
> This project differentiates by keeping a single-binary Go core with an optional separate frontend.

## Status legend

- [x] Done
- [ ] Todo
- [~] In progress

## Phase 1 — Multi-upstream routing  `[x] Done`  (merged 2026-06-22)

**Goal:** Replace the hardcoded single upstream with a model-based router supporting multiple OpenAI-compatible providers.

Delivered:
- `internal/router`: immutable, concurrency-safe `Router` mapping model names to upstreams with alias resolution
- `internal/config`: `upstreams[]` schema with per-upstream model lists, alias maps, `routing.timeout`; strict YAML decoding
- `internal/db`: `upstream` column on `audit_logs` with idempotent migration
- `internal/handler`: per-request upstream selection via context, model-alias body rewrite, 404 on unmatched model, per-request timeout
- Unit tests for config / router / db / handler / middleware under the race detector

Verification: `go fmt`, `go vet`, `go test -race -count=3` all green. Coverage: config/middleware/router 100%, handler 94.2%, db 80%.

## Phase 1.5 — Resilience  `[ ] Todo`

**Goal:** Make routing failures recoverable instead of fatal.

Scope:
- Pre-first-byte retry: when an upstream returns 5xx or a network error before any response body is streamed, retry the request against the same upstream (bounded attempts) or a declared fallback upstream
- Health tracking: per-upstream failure counter with cool-down window; temporarily skip degraded upstreams when alternates exist
- Request body buffering discipline: only buffer what is needed for retry; never hold the full streaming response
- Config additions: `upstreams[].fallback`, `routing.max_retries`, `routing.retry_backoff`

Out of scope: mid-stream retry (impossible without buffering the entire response), circuit breaker with sliding windows (defer to Phase 4 observability work).

Exit criteria:
- A failing upstream with a configured fallback serves the request successfully
- Tests cover retry-then-success, retry-then-fallback, and retry-exhausted paths
- `go test -race -count=3` green

## Phase 2 — Usage governance  `[ ] Todo`

**Goal:** Move from static YAML keys to managed, rate-limited, quota-enforced API keys.

Scope:
- `internal/auth`: persist gateway API keys to SQLite with hashed secrets (`crypto/sha256` + per-key salt); support generate / list / revoke via HTTP
- `internal/middleware`: rate limiting per key using `golang.org/x/time/rate` (token bucket); configurable RPM/TPM limits
- Quota tracking: per-key daily / monthly token budget enforced at audit-write time; reject requests over quota with `429`
- Audit query API: `GET /v1/audit/logs` with key, model, upstream, time-range filters; paginated
- Admin endpoints separated from gateway endpoints under a distinct auth scope

Config additions:
```yaml
gateway:
  keys_file: "gateway.db"   # switch from static list to DB-backed
  rate_limit:
    default_rpm: 60
    default_tpm: 100000
  quota:
    default_daily_tokens: 1000000
```

Exit criteria:
- New `gateway admin create-key` flow generates a key that passes the existing auth middleware
- Requests over the configured rate limit receive `429` with `Retry-After`
- Audit logs are queryable via HTTP without touching SQLite directly
- Race detector clean under concurrent key creation and request load

## Phase 3 — Observability and deployment  `[ ] Todo`

**Goal:** Make the gateway production-grade: measurable, containerized, and CI-verified.

Scope:
- Prometheus metrics endpoint (`GET /metrics`): request count, latency histogram, token usage, per-upstream error rate, in-flight requests
- Structured logging enrichment: add `request_id`, `key_id` (hashed), `upstream` to every log line
- Graceful shutdown: `SIGTERM` triggers `Server.Shutdown` with a deadline; in-flight streams complete, new requests rejected with `503`
- `Dockerfile` (multi-stage, scratch or distroless final image, single binary)
- `docker-compose.yml` for local dev (gateway + optional Postgres)
- GitHub Actions CI: `fmt + vet + test -race + build` on push; release workflow producing cross-platform binaries via `goreleaser` or matrix builds
- Config loading: support `GATEWAY_CONFIG` env var override; keep `config.yaml` as default

Exit criteria:
- `docker compose up` starts a working gateway reachable on the configured port
- CI pipeline is green on `master` and on PRs
- `curl /metrics` returns Prometheus-format metrics after a sample request
- `kill -TERM` allows in-flight requests to finish within the shutdown deadline

## Phase 4 — Provider extensibility and guardrails  `[ ] Todo`

**Goal:** Support non-OpenAI providers natively and add request/response policy controls.

Scope:
- Provider adapter interface in `internal/provider`: translate between the OpenAI wire format and native provider APIs (Anthropic `/v1/messages`, Google Vertex, Cohere)
- Per-upstream `type: openai | anthropic | vertex` field; adapter selected at routing time
- Guardrails: configurable request filters (regex / keyword blocklist on prompt content) and response filters (PII redaction, toxicity score thresholds via pluggable checker)
- Response caching: optional in-memory or SQLite-backed cache keyed by `(upstream, model, messages_hash)` with TTL
- Streaming-aware retry extended to adapter boundary

Exit criteria:
- A request for `claude-3-5-sonnet` routes to an Anthropic upstream and returns an OpenAI-format response to the client
- A blocked prompt returns `400` with a policy reason without hitting the upstream
- Cache hit serves an identical response in `<5ms` with `X-Cache: HIT` header

## Phase 5 — Separate frontend (dashboard)  `[ ] Todo`

**Goal:** Provide a web UI for observability and key management, matching the theopenco/llmgateway dashboard surface area but as an independent frontend talking to the Go gateway.

Scope:
- Next.js (or SvelteKit) app under `web/` consuming the gateway's HTTP API
- Views: audit log explorer, per-key usage charts, upstream health, key management, model/alias configuration viewer (read-only)
- Shipped as a separate deployable; gateway embeds only an optional redirect to the dashboard URL
- Authentication shared via the gateway API key (hashed) or a separate dashboard session

Out of scope: bundling the frontend into the Go binary (would break the single-binary ethos; revisit if `embed` + HTMX is preferred later).

Exit criteria:
- Dashboard displays the last 100 audit rows with filters by key / model / upstream
- A new API key can be created from the UI and immediately used against the gateway
- Frontend builds and runs independently via `pnpm dev` in `web/`

## Phase 6 — Pluggable persistence  `[ ] Todo`

**Goal:** Allow swapping SQLite for Postgres for high-volume deployments without forking.

Scope:
- Abstract `internal/db` behind a `Store` interface: `Init`, `InsertAsync`, `QueryAuditLogs`, key CRUD
- SQLite implementation remains the zero-dependency default
- Postgres implementation via `pgx`/`database/sql`; selected by `storage.driver: sqlite | postgres`
- Migration runner supporting both drivers
- Documented operational tradeoffs: SQLite (single-binary, low volume) vs Postgres (high concurrency, HA)

Exit criteria:
- A gateway started with `storage.driver: postgres` persists audit logs to Postgres and passes the full test suite against a real Postgres instance (integration-tagged tests)
- Switching back to SQLite requires only a config change

## Non-goals (explicit)

These are deliberately out of scope to keep the project focused:

- **Hosted/SaaS offering** — theopenco/llmgateway offers a hosted version; this project is self-host only
- **Billing / subscription management** — out of scope for an open-source gateway; integrate externally if needed
- **Multi-tenant organizations / teams** — single-tenant self-host; team features belong in an external control plane
- **LLM inference itself** — this is a gateway, not a model runner; Ollama/vLLM are upstreams, not competitors

## Versioning and release cadence

- SemVer via conventional commits (see `conventional-git` skill)
- Phase 1.5 → `v0.2.0`
- Phase 2 → `v0.3.0`
- Phase 3 → `v1.0.0` (first "production-ready" tag)
- Later phases → minor bumps until the interface stabilizes

## How to propose changes to this roadmap

Open an issue with the `roadmap` label. Proposals should include: which phase is affected, the problem being solved, and a sketch of the config / API surface change. Breaking config changes require a migration section in the proposal.
