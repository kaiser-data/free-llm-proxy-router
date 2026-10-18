# free-llm-proxy-router

[![CI](https://github.com/kaiser-data/free-llm-proxy-router/actions/workflows/ci.yml/badge.svg)](https://github.com/kaiser-data/free-llm-proxy-router/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/go-1.23+-00ADD8?logo=go)](go.mod)
[![License: MIT](https://img.shields.io/badge/license-MIT-green)](#license)

**One small Go binary that turns every free LLM tier you have into a single OpenAI endpoint that doesn't run dry.**

Point any OpenAI-compatible tool at `localhost:8080`. When Groq hits its rate limit, the request moves to Gemini, then Cerebras, then OpenRouter's free pool, and your script never sees a 429. No Node, no database, no dashboard, no credit card.

```bash
export OPENAI_BASE_URL=http://localhost:8080/v1 OPENAI_API_KEY=unused
aider --model openai/coding          # or Continue, Cline, OpenCode, Open WebUI, LangChain…
```

## Why this instead of a big gateway?

Full AI gateways such as [OmniRoute](https://github.com/diegosouzapw/OmniRoute) and LiteLLM are great if you want hundreds of providers, paid-key budgets and a web dashboard. This project is deliberately the opposite:

| | free-llm-proxy-router | Full AI gateways |
|---|---|---|
| Install | one static binary, roughly 10 MB | Node/Python runtime, Docker, often a DB |
| Scope | **free tiers only**: official, recharging, no card | paid + free + subscription logins |
| Terms of service | official API keys only, no OAuth or session reuse | some route through consumer subscriptions |
| Code you have to trust | ~7k lines of Go, readable in an afternoon | very large codebases |
| Free-model catalog | auto-discovered from provider APIs, [shared as JSON](configs/catalog.json) | curated lists |
| Runs on | laptop, Raspberry Pi, NAS, $4 VPS, CI runner | servers |

Pick this when you want free inference for scripts, agents, homelabs or CI without operating a platform.

## What it does

- **Single endpoint** at `localhost:8080` — drop-in replacement for the OpenAI API
- **Auto-routes** to the best available free model using a configurable strategy
- **Falls back** automatically when a provider is rate-limited or unavailable
- **Streams** to clients that send `stream: true`. Providers are called buffered so fallback still works, and the final answer is sent back as standard SSE chunks
- **Discovers** free models by scanning provider APIs (no hardcoded lists). Browse the current list in [FREE_MODELS.md](FREE_MODELS.md), refreshed weekly
- **Tracks** per-provider reliability and rate limits across requests

## Binaries

| Binary | Purpose |
|--------|---------|
| `free-llm-proxy` | OpenAI-compatible proxy server |
| `free-llm-scan` | Free model discovery and catalog management |
| `free-llm-bench` | Benchmark all strategies across all providers |

## Quick start

### 1. Install

**Prebuilt binary** (Linux, macOS, Windows; amd64 and arm64): grab an archive from [Releases](https://github.com/kaiser-data/free-llm-proxy-router/releases/latest).

```bash
# Example: macOS on Apple Silicon
VERSION=0.1.0
curl -LO https://github.com/kaiser-data/free-llm-proxy-router/releases/download/v$VERSION/free-llm-proxy-router_${VERSION}_darwin_arm64.tar.gz
tar xzf free-llm-proxy-router_${VERSION}_darwin_arm64.tar.gz
./free-llm-proxy --help
```

**From source:**

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
