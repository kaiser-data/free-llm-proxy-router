# free-llm-proxy-router

An OpenAI-compatible local proxy that routes LLM requests across free-tier providers — Groq, Gemini, OpenRouter, UnoRouter, GitHub Models, Cerebras, Mistral, HuggingFace and more — with automatic fallback, rate-limit recovery, and 13 routing strategies.

## What it does

- **Single endpoint** at `localhost:8080` — drop-in replacement for the OpenAI API
- **Auto-routes** to the best available free model using a configurable strategy
- **Falls back** automatically when a provider is rate-limited or unavailable
- **Discovers** free models by scanning provider APIs (no hardcoded lists)
- **Tracks** per-provider reliability and rate limits across requests

## Binaries

| Binary | Purpose |
|--------|---------|
| `free-llm-proxy` | OpenAI-compatible proxy server |
| `free-llm-scan` | Free model discovery and catalog management |
| `free-llm-bench` | Benchmark all strategies across all providers |

## Quick start

### 1. Build

```bash
git clone https://github.com/kaiser-data/free-llm-proxy-router
cd free-llm-proxy-router

# Requires Go 1.23+
make build
# Binaries land in bin/
```

### 2. Add API keys

```bash
cp .env.example .env
# Edit .env and fill in your keys
```

At minimum you need one key. Easiest to get (no credit card):

| Provider | Sign up | Free tier |
|----------|---------|-----------|
| **Groq** | https://console.groq.com/keys | Fast inference, all models free |
| **Gemini** | https://aistudio.google.com/app/apikey | 1500 req/day, 1M context |
| **OpenRouter** | https://openrouter.ai/settings/keys | 50+ free models via one key |
| **GitHub Models** | https://github.com/settings/tokens | Any GitHub PAT, vision models included |

### 3. Discover free models

```bash
./bin/free-llm-scan update
# Writes ~/.free-llm-proxy-router/catalog.json
```

### 4. Start the proxy

```bash
./bin/free-llm-proxy
# Listening on http://localhost:8080
```

### 5. Send requests

```bash
# Auto strategy — picks the best available model
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"auto","messages":[{"role":"user","content":"Hello!"}]}'

# List all discovered free models
curl http://localhost:8080/v1/models
```

## Routing strategies

Pass any strategy name as the `model` field, or set a default in `config.yaml`:

| Strategy | Description |
|----------|-------------|
| `auto` / `adaptive` | Reliability-weighted, adjusts over time |
| `performance` | Largest model available (best quality) |
| `speed` | Fastest provider first (Cerebras > Groq > others) |
| `volume` | Highest daily request limit (Gemini Flash-Lite) |
| `balanced` | 14–79B parameter models only |
| `small` | 3–13B models — fast and efficient |
| `tiny` | <3B models — ultra-fast, low latency |
| `coding` | Prefers models with code capability |
| `long_context` | Highest context window first (Gemini 1M) |
| `similar` | Same model family as a reference model |
| `parallel` | Returns N models for fan-out requests |
| `reliable` | Highest success-rate providers first |
| `economical` | Rate-limited free tiers before credit-based |

## Fallback chain

Every request runs through a 5-step fallback:

1. **OpenRouter** with `models[]` array — native multi-model fallback in one request
2. **Groq** — fast inference, free tier, per-model RPM/RPD limits
3. **Gemini** (4-dimensional rate limit check: RPM/TPM/RPD/IPM)
4. **Remaining ranked providers** (by strategy)
5. **OpenRouter `openrouter/free`** — ultimate fallback router

Rate-limited providers (429) are put on a cooldown automatically and skipped until the window resets — no blocking, no sleep.

## Supported providers

All providers have recharging free limits — no one-time trial credits, no credit card required.

| Provider | Free limit | Resets |
|----------|------------|--------|
| **OpenRouter** | 50+ free models (pricing == $0) | always free |
| **Groq** | Per-model RPM/RPD | daily |
| **Google AI Studio** | 1500 req/day, 1M token context | midnight PT |
| **GitHub Models** | 150–500 req/day (low/high tier) | daily |
| **Cerebras** | Per-second RPS limits | continuously |
| **HuggingFace** | 300 calls/hour | hourly |
| **Mistral AI** | 2 RPM | per minute |
| **Cohere** | 1000 req/month | monthly |
| **NVIDIA NIM** | Free credits, 40 RPM, 200+ models | monthly |
| **UnoRouter** | Free models only (IDs with `:free` suffix, e.g. `qwen/qwen3-235b-a22b:free`) | always free |
| **Ollama** | Unlimited | local, no key |

### UnoRouter (generic OpenAI-compatible upstream)

UnoRouter is an independent OpenAI-compatible aggregator — **not** OpenRouter.
It is supported as its own upstream provider through the standard
OpenAI-compatible wire format:

- **Base URL** — `https://api.unorouter.com/v1` (config key `unorouter`)
- **Auth** — `Authorization: Bearer $UNOROUTER_API_KEY`; the key comes from
  the environment (`api_key_env: UNOROUTER_API_KEY`), never hardcoded. The
  literal form `api_key: "${UNOROUTER_API_KEY}"` also works — `${VAR}`
  references in `api_key` are expanded from the environment at load time.
- **Discovery** — `GET /v1/models` (OpenAI-compatible listing; availability
  depends on the API key). Results are plain catalog entries, so they flow
  through the same canonicalization, routing, health, and fallback machinery
  as every other provider — no separate model registry.
- **Free detection** — pricing metadata is preferred when the API exposes it
  (zero prompt+completion / input+output prices, or an explicit free flag);
  otherwise the documented `:free` ID suffix decides (`discovery.free_markers`,
  default `[":free"]`). Models with neither signal are treated as paid and
  excluded — not every UnoRouter model is free.
- **Canonical aliases** — UnoRouter free IDs canonicalize like any other
  provider (`qwen/qwen3-235b-a22b:free` and `qwen/qwen3-30b-a3b:free` join
  the `qwen3` group), so clients request `qwen3` while the upstream receives
  the exact raw `:free` ID. Multiple free candidates for one canonical name
  walk the existing retryable-failure fallback chain.
- **Standalone** — UnoRouter works as the only enabled upstream when just
  `UNOROUTER_API_KEY` is configured.

The provider list may also be spelled `upstreams:` in the config file
(`providers:` wins when both keys are present):

```yaml
upstreams:
  - id: unorouter
    base_url: https://api.unorouter.com/v1
    api_key: ${UNOROUTER_API_KEY}
```

## Configuration

Config is loaded from `~/.free-llm-proxy-router/config.yaml` (created by `setup.sh`).
Hot-reloaded via fsnotify — no restart needed when you save the file.

```bash
bash scripts/setup.sh   # First-time setup
```

Key settings in `config.yaml`:

```yaml
proxy:
  strategy: "adaptive"   # Default routing strategy
  port: 8080

catalog:
  max_age_hours: 24      # Re-scan after this many hours
```

### Canonical model aliases

A canonical name (e.g. `qwen3`) can map to one or more raw upstream model IDs.
A request for the canonical name tries the mapped upstreams in order and uses
the first free success; the response body is returned verbatim and the
`X-Used-Model` header reports the canonical name. Raw model IDs keep working
unchanged. Canonical names are also advertised in `GET /v1/models` so clients
can discover them like any other model.

```yaml
models:
  aliases:
    # OpenRouter's free Qwen3 235B first, Groq's qwen3-32b as backup.
    qwen3:
      - "qwen/qwen3-235b-a22b:free"
      - "qwen3-32b"
  canonicalization:
    enabled: true    # Auto-derive groups from the catalog by model family
    free_only: true  # Only map free upstream models
  # What GET /v1/models advertises to clients:
  #   expose_raw       — raw upstream IDs ("Qwen/Qwen3-32B", …)
  #   expose_canonical — canonical alias names ("qwen3", …)
  # Defaults: both true (backward compatible). Set expose_raw: false to
  # advertise canonical names only. Raw IDs stay requestable and keep
  # driving routing/debugging internally either way — these flags only
  # filter the /v1/models listing. Disabling both is treated as a
  # misconfiguration and both are listed.
  expose_raw: true
  expose_canonical: true
```

### Single-model public mode (`kiwi-auto`)

For production deployments where clients should never care which free model
serves them, `models.public_alias` turns ONE model name into the entire pool
of discovered free chat-capable models:

```yaml
models:
  public_alias: "kiwi-auto"   # one name for the whole free pool
  expose_raw: false
  expose_canonical: false
```

With this configuration `GET /v1/models` advertises exactly one entry —
`kiwi-auto` — and every chat request naming `kiwi-auto` is routed to the best
candidate among **all** currently discovered free chat-capable models
(`qwen3:free`, `deepseek-v3:free`, `gemma3:free`, …). Semantics:

- **Dynamic** — the pool is rebuilt from the live catalog on every catalog
  refresh; models appearing or disappearing never require a client change.
  The client always sends `model: "kiwi-auto"`.
- **Free-only and chat-capable** — paid models never enter the pool, and
  non-chat models (embedding/image/speech, by endpoint metadata or name
  pattern) are excluded. Eligibility stays with the existing discovery
  layer (`:free` markers / pricing metadata / endpoint types).
- **Existing routing machinery** — pool candidates are ranked by the
  configured strategy (`proxy.strategy`, default `adaptive`), and candidates
  whose recorded context window cannot hold the request are demoted behind
  fitting ones. Health, cooldown, 429 handling, retryable-5xx fallback, and
  timeouts behave exactly as on the canonical-alias route.
- **Streaming** — `model: "kiwi-auto", stream: true` uses the real SSE
  pass-through: one upstream candidate is selected, its event stream is
  pumped byte-for-byte, and fallback between candidates happens only before
  the first downstream byte.
- **Debug headers** — `X-Used-Model: kiwi-auto` while
  `X-Free-Router-Upstream-Provider` / `X-Free-Router-Upstream-Model` reveal
  the actual upstream that served the request.
- **Raw compatibility** — raw IDs (`unorouter/qwen3:free`) and canonical
  family names (`qwen3`) remain requestable for advanced/raw use but are not
  advertised in `/v1/models` in this mode.
- **Operator visibility** — `GET /debug/pool` reports the live candidate set
  behind the alias (provider, model, context window) without exposing
  credentials.

### Error classification

Failures during candidate routing are classified once, and the class decides
between "try the next candidate" and "return immediately":

- **Retryable** — `408`, `429`, all `5xx`, upstream timeouts, and connection
  failures: the same candidate is first retried up to
  `fallback.retries_per_candidate` times (default 1) with exponential
  backoff — a 429 retry gated by `fallback.retry_on_429`, a 5xx retry by
  `fallback.retry_on_5xx` — and then the walk continues to the next
  candidate (per-status cooldowns still apply). Retries consume the
  `max_attempts` budget. Only when every candidate fails with a retryable
  failure does the request get the short exhaustion response.
- **Non-retryable** — `400`, `401`, `403`, and every other `4xx` except
  `404`/`408`/`429`: the upstream response is returned to the client
  verbatim. No further candidates are walked and a client error is never
  masked behind a fake successful response.

`404` is deliberately treated as retryable: during a candidate walk it means
"this provider does not serve this model" (catalog staleness), so other
candidates are tried and the entry is flagged for reverification. A `404`
caused by request semantics — a model that is neither in the catalog nor a
canonical alias — is answered immediately by the proxy itself, before any
upstream call. Note that upstream `401`/`403` stem from the proxy's own
provider credentials; they are surfaced instead of silently retried so a
misconfiguration is visible, and the provider is put on a 10-minute cooldown
either way.

## Catalog management

```bash
# Scan all providers for free models
./bin/free-llm-scan update

# Probe models flagged as needing reverification (got a 429)
./bin/free-llm-scan probe

# LLM-powered diff refresh (checks for free tier changes)
./bin/free-llm-scan refresh-llm

# Weekly cron refresh
crontab -e
# Add: 0 9 * * 1 /path/to/free-llm-proxy-router/scripts/cron-refresh.sh
```

## API key files

Keys are loaded from (first match wins):

1. Real environment variable (`export GROQ_API_KEY=...`)
2. `.env` in the project directory
3. `~/.free-llm-proxy-router/.secrets`

The `.env` file is gitignored. See `.env.example` for all supported keys.

## Known limitations

- **Streaming (`stream: true`) is proxied as real SSE on the raw-model and
  canonical-alias routes.** When the upstream answers `text/event-stream`,
  the proxy pumps the bytes through incrementally (per-read flush) — clients
  see `data: {...}` chunks and `data: [DONE]` as the upstream produces them.
  Fallback between candidates happens only before the first downstream byte;
  once a stream has started, a mid-stream failure truncates the stream rather
  than splicing in another candidate's generation. If the client disconnects,
  the upstream request context is canceled so no inference keeps running.
  Upstreams that answer `stream: true` with a plain JSON body (ignoring the
  flag) are forwarded as `application/json` — JSON is never disguised as an
  event stream. Remaining scope: requests that reach the strategy chain
  (`model: "auto"` / strategy names, or an alias/raw route whose candidates
  all failed) still return buffered JSON. Streaming attempts are bounded by
  `fallback.stream_timeout_seconds` (default: inherit
  `fallback.provider_timeout_seconds`, itself 120s) — configurable, since a
  fixed 120s cap silently truncated long streams mid-generation.

## Development

```bash
# Run tests
go test ./...

# Vet
go vet ./...

# Build all binaries
make build
```

Tests cover: 30+ model name classifier cases, all 10 provider rate-limit header extractors, all 13 strategy `Rank()` methods.

## Acknowledgments

Provider rate limit data, free tier detection, and free-tier change tracking informed by:

- **[cheahjs/free-llm-api-resources](https://github.com/cheahjs/free-llm-api-resources)** — comprehensive community-maintained reference for free LLM API tiers, rate limits, and supported models across all major providers. The live limits comment in `config.yaml` links back to this resource.

## License

MIT
