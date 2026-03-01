# picoclaw-free-llm

An OpenAI-compatible local proxy that routes LLM requests across free-tier providers — Groq, Gemini, OpenRouter, Cerebras, Mistral, NVIDIA NIM, HuggingFace and more — with automatic fallback, rate-limit awareness, and 13 routing strategies.

## What it does

- **Single endpoint** at `localhost:8080` — drop-in replacement for the OpenAI API
- **Auto-routes** to the best available free model using a configurable strategy
- **Falls back** automatically when a provider is rate-limited or unavailable
- **Discovers** free models by scanning provider APIs (no hardcoded lists)
- **Tracks** per-provider reliability and rate limits across requests

## Binaries

| Binary | Purpose |
|--------|---------|
| `picoclaw-proxy` | OpenAI-compatible proxy server |
| `picoclaw-scan` | Free model discovery and catalog management |
| `picoclaw-bench` | Benchmark all strategies across all providers |

## Quick start

### 1. Build

```bash
git clone https://github.com/kaiser-data/picoclaw-free-llm
cd picoclaw-free-llm

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

### 3. Discover free models

```bash
./bin/picoclaw-scan update
# Writes ~/.picoclaw-free-llm/catalog.json
```

### 4. Start the proxy

```bash
./bin/picoclaw-proxy
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
2. **Groq** with `service_tier: auto`
3. **Gemini** (4-dimensional rate limit check: RPM/TPM/RPD/IPM)
4. **Remaining ranked providers** (by strategy)
5. **OpenRouter `openrouter/free`** — ultimate fallback router

## Supported providers

| Provider | Type | Notes |
|----------|------|-------|
| OpenRouter | Free | 50+ models, native fallback, `:free` suffix detection |
| Groq | Free | All models free, per-model RPM/RPD limits |
| Google AI Studio | Free | Gemini models, 1500 RPD, resets midnight PT |
| Mistral AI | Free | 2 RPM free tier |
| Cerebras | Free | Hardware-accelerated, RPS limits |
| NVIDIA NIM | Credits | 40 RPM, free credits on signup |
| HuggingFace | Free | 300 calls/hour, warm models only |
| Together AI | Credits | Free $1 credit on signup |
| DeepSeek | Credits | R1 reasoning model, free credits |
| Cohere | Free | 1000 req/month |
| Ollama | Local | No key needed, runs models locally |

## Configuration

Config is loaded from `~/.picoclaw-free-llm/config.yaml` (created by `setup.sh`).
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
./bin/picoclaw-scan update

# Probe models flagged as needing reverification (got a 429)
./bin/picoclaw-scan probe

# LLM-powered diff refresh (checks for free tier changes)
./bin/picoclaw-scan refresh-llm

# Weekly cron refresh
crontab -e
# Add: 0 9 * * 1 /path/to/picoclaw-free-llm/scripts/cron-refresh.sh
```

## API key files

Keys are loaded from (first match wins):

1. Real environment variable (`export GROQ_API_KEY=...`)
2. `.env` in the project directory
3. `~/.picoclaw-free-llm/.secrets`

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

## License

MIT
