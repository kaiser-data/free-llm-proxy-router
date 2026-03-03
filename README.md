# free-llm-proxy-router

An OpenAI-compatible local proxy that routes LLM requests across free-tier providers — Groq, Gemini, OpenRouter, GitHub Models, Cerebras, Mistral, HuggingFace and more — with automatic fallback, rate-limit recovery, and 13 routing strategies.

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
| **Ollama** | Unlimited | local, no key |

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
