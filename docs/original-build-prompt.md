# Claude Code Prompt: Build `picoclaw-free-llm`

> Paste this entire prompt into Claude Code to scaffold the full repo from scratch.
> This is a living system designed to stay correct even as providers change.

---

## North Star Principles

**1. Total Provider Agnosticism** — The system must never assume anything is stable. Model
names, free tiers, rate limits, and entire providers change without warning. The hardcoded
registry is only a bootstrap seed. Everything is discovered at runtime and refreshable.

**2. Exploit Native Provider Features First** — Before implementing any fallback or routing
logic, check whether the provider already handles it natively. OpenRouter has a built-in
free model router and native fallback chains. Groq has a `service_tier` field. Use these
features instead of reimplementing them. Native beats custom.

**3. Read the Docs Before Touching Code** — For each provider, fetch and read its official
API documentation before writing a single line of implementation. This prompt gives you
research-backed starting points, but documentation may have changed. Trust the live docs.

---

## Provider Research Protocol

**This is mandatory. Before implementing any provider, execute these steps:**

```
1. Fetch the provider's official API docs (URLs provided below per provider)
2. Read the rate limit page — note every header they send on 429
3. Read the models page — note how free models are identified
4. Check for any native fallback or routing features
5. Note the exact error codes, error body format, and retry guidance
6. Implement ONLY what the docs confirm is current
```

If a doc URL returns 404 or contradicts this prompt, **trust the live docs, not this prompt**.

---

## What This Builds

Three binaries for the [PicoClaw](https://github.com/sipeed/picoclaw) ecosystem:

1. **`picoclaw-proxy`** — OpenAI-compatible local API tunnel. Intelligent routing strategies,
   native provider fallback exploitation, live model discovery, automatic failover.

2. **`picoclaw-scan`** — Discovery tool. Probes provider APIs for free models, extracts
   parameter counts, correlates identical models across providers, refreshes via LLM.

3. **`picoclaw-bench`** — Benchmark runner. Tests all discovered models across all strategies,
   outputs ranked results per strategy.

PicoClaw points to `http://localhost:8080/v1`. The proxy handles everything else.

---

## Provider-Specific Implementation Guide

### ⚠️ Read this section entirely before writing any provider code

Each provider has unique behavior around free model detection, rate limiting, headers,
and native routing features. Implementing them generically loses valuable capabilities.

---

### Provider 1: OpenRouter

**Docs to read first:**
- https://openrouter.ai/docs/quickstart
- https://openrouter.ai/docs/guides/routing/model-fallbacks
- https://openrouter.ai/docs/guides/routing/routers/free-models-router
- https://openrouter.ai/docs/guides/routing/routers/auto-router
- https://openrouter.ai/docs/guides/routing/provider-selection
- https://openrouter.ai/docs/faq (rate limits section)

**Known native features to exploit:**

```
NATIVE FALLBACK — OpenRouter supports a `models` array in the request body.
It tries them in order automatically. Use this to offload internal fallback to OpenRouter:

  POST /v1/chat/completions
  {
    "model": "meta-llama/llama-3.1-8b-instruct:free",
    "models": [
      "meta-llama/llama-3.1-8b-instruct:free",
      "google/gemma-2-9b-it:free",
      "mistralai/mistral-7b-instruct:free"
    ],
    ...
  }

The actual model used is returned in the response body `model` field. Log this.

FREE MODELS ROUTER — Use model ID "openrouter/free" to let OpenRouter randomly select
a free model. It filters for capability compatibility automatically. Use this as the
ultimate fallback when all specific free models are exhausted.

MODEL VARIANTS (append to any model ID):
  :free       — free tier variant of a model
  :nitro      — route to fastest provider for this model
  :floor      — route to cheapest provider
  :online     — enable web search for this request
  :thinking   — enable reasoning mode

PRICING FIELD — The /v1/models response includes a `pricing` object.
  Free models have: pricing.prompt == "0" AND pricing.completion == "0"
  Use this for discovery, not just the :free suffix.

PROVIDER ROUTING — Request body supports a `provider` object:
  { "provider": { "order": ["Groq", "Together"], "allow_fallbacks": true } }
  Use this to control which backend providers OpenRouter tries.
```

**Rate limit behavior:**
- 50 req/day on free models without balance
- 1000 req/day with $10+ balance added
- Standard 429 response, standard `Retry-After` header
- Limits are per account, not per model

**Required headers (always send for all requests):**
```
HTTP-Referer: https://github.com/your-username/picoclaw-free-llm
X-Title: PicoClaw Free LLM
```

**Discovery method:** Call `GET /v1/models`, filter where `pricing.prompt == "0"`.
Also detect `:free` suffix as secondary signal. Cross-reference both.

---

### Provider 2: Groq

**Docs to read first:**
- https://console.groq.com/docs/rate-limits
- https://console.groq.com/docs/api-reference
- https://console.groq.com/docs/models

**Known native features to exploit:**

```
SERVICE TIER — Groq supports a `service_tier` field in the request body:
  "on_demand"  — default, uses current account limits
  "auto"       — automatically selects the highest available tier
  "flex"       — will succeed or fail quickly (no queuing)

Always send `"service_tier": "auto"` to maximize available capacity.

WHISPER — Groq offers free Whisper voice transcription. Separate endpoint.
  POST /openai/v1/audio/transcriptions
  This is relevant for PicoClaw's Telegram voice message support.
```

**Rate limit behavior:**
- Per-model limits (each model has its own RPM/RPD/TPM bucket)
- Standard 429 response
- No native multi-model fallback — implement manually
- Limits are at organization level, not per API key
- No `Retry-After` header documented — use exponential backoff with jitter

**Discovery method:** `GET /openai/v1/models`. All models returned are on the free tier
(Groq's free account access covers all listed models). Check the docs for any paid-only
models before marking everything as free.

**Important:** Groq inference is very fast (300-750+ tok/s). Because of this, it's easy
to hit RPM limits even with moderate traffic. The rate limiter must track per-model,
not per-provider, for Groq.

---

### Provider 3: Gemini (Google AI Studio)

**Docs to read first:**
- https://ai.google.dev/gemini-api/docs/rate-limits
- https://ai.google.dev/gemini-api/docs/models
- https://aistudio.google.com/app/apikey (rate limit table per model)

**Known behavior (critical — different from all other providers):**

```
FOUR-DIMENSIONAL RATE LIMITING — Gemini enforces limits across 4 independent dimensions:
  RPM  — requests per minute
  TPM  — tokens per minute
  RPD  — requests per day
  IPM  — images per minute (only for image-capable models)

Exceeding ANY single dimension triggers a 429. A single large request can exhaust TPM
even if RPM/RPD are fine. Track all four independently.

PER PROJECT, NOT PER KEY — All API keys in the same Google Cloud project share quota.
This cannot be worked around with multiple keys from the same project.

ERROR TYPE — 429 response body contains: {"error": {"code": 429, "status": "RESOURCE_EXHAUSTED"}}
The status field is "RESOURCE_EXHAUSTED", not generic. Parse this to distinguish from
other providers.

RESPONSE HEADERS — Gemini sends these on 429:
  X-RateLimit-Limit: 15
  X-RateLimit-Remaining: 0
  X-RateLimit-Reset: <unix timestamp>
  Retry-After: 60   (seconds to wait)
ALWAYS read Retry-After before falling back — it tells you exactly when the limit resets.

DAILY RESET — RPD resets at midnight PACIFIC TIME (not UTC). Account for PST/PDT offset
in the rate limiter's daily window calculation.

DECEMBER 2025 CUTS — Google slashed free tier quotas 50-92% on Dec 7, 2025 with no
announcement. Current limits (verify in docs — these change):
  Gemini 2.0 Flash: ~10 RPM, ~20 RPD (was 250 RPD before Dec 2025)
  Gemini Flash-Lite: ~15 RPM, ~1000 RPD (most permissive)
  Gemini 2.5 Pro: ~5 RPM, ~100 RPD
Do NOT use 2024 limits. Fetch current limits from docs before implementing.

MODEL SELECTION — For volume/small strategies, prefer Gemini Flash-Lite (highest RPD).
For performance/long-context, use Gemini 2.0 Flash (1M context window).
```

**Auth:** Does NOT use `Authorization: Bearer` header. Uses:
```
x-goog-api-key: YOUR_API_KEY
```
OpenAI-compat endpoint: `https://generativelanguage.googleapis.com/v1beta/openai/`
This endpoint uses the same `x-goog-api-key` header, not Bearer.

**Discovery:** `GET {base_url}/models`. Returns `models[]` array. Free models are
all models available through AI Studio with a free API key. Parse `supportedGenerationMethods`
to identify chat-capable models.

---

### Provider 4: Mistral

**Docs to read first:**
- https://docs.mistral.ai/deployment/ai-studio/tier
- https://docs.mistral.ai/api/ (rate limits section)
- https://help.mistral.ai/en/articles/424390-how-do-api-rate-limits-work

**Known behavior:**

```
WORKSPACE-LEVEL LIMITS — Like Gemini, Mistral limits are at workspace level, not key level.
Multiple API keys from the same workspace share the same quota pool.

FREE TIER LIMITS (verify in docs — these change):
  2 RPM (very conservative — this is NOT a typo)
  500K TPM
  ~1B tokens/month total

RETRY-AFTER HEADER — Mistral sends Retry-After on 429. ALWAYS read it.
  delay = parse(response.headers["retry-after"]) * 1000  // milliseconds

ERROR FORMAT: Standard HTTP 429 with JSON body. No special error type field.

RETRYABLE ERRORS: 429 and 5xx. Do NOT retry 4xx client errors other than 429.

TOKEN ESTIMATION — Mistral's free TPM limit is generous (500K/min) but RPM is tiny (2/min).
The bottleneck is almost always RPM, not TPM. Don't optimize for token count on Mistral.
```

**Discovery:** `GET /v1/models`. The free tier includes specific models — check the AI Studio
model list for what's free vs paid. Typically `mistral-small-latest` and older open models.

---

### Provider 5: Cerebras

**Docs to read first:**
- https://inference-docs.cerebras.ai/support/rate-limits
- https://inference-docs.cerebras.ai/api-reference/chat-completions

**Known behavior (critical — unique rate limit headers):**

```
RPS LIMITS — Cerebras enforces Requests Per SECOND (not per minute) because inference
is so fast (up to 2600 tok/s) that RPM limits would be hit instantly. Track in seconds.

CUSTOM RATE LIMIT HEADERS — Cerebras injects these on EVERY response (not just 429):
  x-ratelimit-limit-requests
  x-ratelimit-remaining-requests
  x-ratelimit-reset-requests        (timestamp)
  x-ratelimit-limit-tokens
  x-ratelimit-remaining-tokens
  x-ratelimit-reset-tokens-minute   (CRITICAL: fractional seconds until token bucket refills)

Parse x-ratelimit-reset-tokens-minute to compute exact wait time, not generic backoff.
Example: value "0.5" means wait 500ms. Value "2.3" means wait 2300ms.

TCP WARMING — Cerebras SDK sends warm-up requests to /v1/tcp_warming on init to reduce
TTFT. Do NOT implement this in the Go client unless you are keeping a long-lived connection.
Avoid repeatedly reconstructing the HTTP client — reuse it.

BURST HANDLING — Tools sending rapid request bursts (e.g. agentic loops) easily hit
Cerebras RPS limits. Add request spacing of at least 100ms between sequential Cerebras calls
even when not rate limited. This is documented in their FAQ.

FREE TIER — Available for registered users. Daily/hourly limits apply for free accounts.
Check dashboard for current limits — they are not consistently documented.
```

**Discovery:** `GET /v1/models`. All returned models are accessible on free accounts
(subject to rate limits). No special free/paid distinction in the model list.

---

### Provider 6: Together AI

**Docs to read first:**
- https://docs.together.ai/docs/rate-limits
- https://docs.together.ai/docs/inference-models

**Known behavior:**
```
CREDIT-BASED — Together gives $25 free credit on signup. There is no "free tier" in the
traditional sense — it's a prepaid credit system. Track credit usage carefully.
Mark this provider as "credit-based" in the catalog, not "rate-limited".

MODEL DISCOVERY — Together's /v1/models endpoint is extensive. Filter by:
  model.type == "chat"  to get chat models
  Check Together's "free models" page for any completely free (non-credit) offerings.

STANDARD OPENAI-COMPAT — No special auth headers. Standard Bearer token.
Standard 429 with Retry-After header.
```

---

### Provider 7: DeepSeek

**Docs to read first:**
- https://platform.deepseek.com/docs (rate limits)
- https://platform.deepseek.com/docs (models)

**Known behavior:**
```
CREDIT-BASED — Like Together AI, DeepSeek gives ~$5 free credit. Not a traditional free tier.
Mark as "credit-based" in catalog.

REASONING MODEL — deepseek-r1 supports a special `thinking` mode where it outputs
reasoning tokens. The response includes a `reasoning_content` field alongside `content`.
Log reasoning token count separately in bench results.

STANDARD OPENAI-COMPAT — Standard Bearer token, standard endpoints.
Standard 429 behavior.
```

---

### Provider 8: Cohere

**Docs to read first:**
- https://docs.cohere.com/reference/rate-limits
- https://docs.cohere.com/reference/versioning

**Known behavior:**
```
TRUE FREE TIER — 1000 req/month on the Trial key. No credit card required.
Monthly limit, not daily. Track month-boundary resets, not daily resets.

VERSIONED API — Cohere uses API versioning via a required header:
  Cohere-Version: 2022-12-06   (or latest — check docs)
This header is required for all requests. Bake it into the Cohere provider config.

COMMAND MODEL — "command-r" and "command-a" are the relevant chat models.
The free Trial key gives access to both.

STANDARD 429 with Retry-After header.
```

---

### Provider 9: NVIDIA NIM

**Docs to read first:**
- https://docs.api.nvidia.com/nim/reference/limits
- https://build.nvidia.com/explore/discover

**Known behavior:**
```
CREDIT-BASED — NVIDIA gives free credits on signup (~1000 credits = ~1000 requests).
Not a traditional ongoing free tier. Mark as "credit-based" in catalog.

RATE LIMITS — 40 RPM hard limit across all models, regardless of credits.
This is the primary constraint, not credits.

MODEL DISCOVERY — The /v1/models endpoint works. Filter for chat models.
Many open-source models are available: Llama, Mistral, Phi, Gemma variants.

STANDARD OPENAI-COMPAT — Standard Bearer token.
```

---

### Provider 10: Hugging Face Inference API

**Docs to read first:**
- https://huggingface.co/docs/api-inference/quicktour
- https://huggingface.co/docs/huggingface_hub/guides/inference

**Known behavior:**
```
GATED MODELS — Many models on HF require acceptance of terms. Do NOT attempt to call
gated models without checking `gated` field in the model metadata first.

FREE TIER LIMITS — 300 API calls per hour per token (reset hourly). This is much lower
than other providers. HF is best used as a last-resort fallback, not a primary provider.

COLD STARTS — Models may be unloaded. A 503 with body containing "loading" means the
model is warming up. Implement a special wait-and-retry for 503+loading (up to 60s wait),
distinct from the normal 5xx error path.

RESPONSE FORMAT — HF returns a different format for some model types. Their OpenAI-compat
endpoint (/v1/chat/completions) is only available for models that explicitly support it.
Check for this support before adding a model to the catalog.

MODEL DISCOVERY — Do NOT call GET /v1/models expecting a manageable list. HF has 100k+
models. Instead, use a curated list of known-good inference-ready models, or use the
HF Hub API with filters: task=text-generation, library=transformers, inference=warm.
Add a special HF discovery mode that uses their Hub API, not the inference /v1/models endpoint.

ERROR 503 special handling:
  if status == 503 AND body contains "currently loading":
    wait estimated_time from body (default 30s), then retry SAME provider
    (unlike all other 5xx errors where we skip to next provider)
```

---

## Rate Limit Header Abstraction

Because every provider uses different rate limit headers, build a unified extractor:

```go
// pkg/ratelimit/headers.go

// RateLimitInfo is extracted from any provider's response headers
type RateLimitInfo struct {
    RetryAfterMs      int64  // from Retry-After (most providers)
    ResetTimestamp    int64  // from X-RateLimit-Reset (Gemini, Cerebras)
    RemainingRequests int    // from X-RateLimit-Remaining (Gemini, Cerebras)
    RemainingTokens   int    // from x-ratelimit-remaining-tokens (Cerebras)
    // CerebrasTokResetMs is fractional seconds from x-ratelimit-reset-tokens-minute
    CerebrasTokResetMs int64
    // GeminiErrorType: "RESOURCE_EXHAUSTED" or empty
    GeminiErrorType   string
    // IsRPDExhausted: true if daily limit hit (must wait until midnight PT, not 60s)
    IsRPDExhausted    bool
}

// ExtractRateLimitInfo reads all known rate limit headers from a 429 response
// and returns a unified struct. Provider-specific logic is here, not scattered.
func ExtractRateLimitInfo(providerID string, headers http.Header, body []byte) RateLimitInfo {
    info := RateLimitInfo{}

    // Universal: Retry-After (present in Mistral, OpenRouter, Cohere, most providers)
    if ra := headers.Get("Retry-After"); ra != "" {
        if secs, err := strconv.ParseInt(ra, 10, 64); err == nil {
            info.RetryAfterMs = secs * 1000
        }
    }

    switch providerID {
    case "gemini":
        // Gemini: check error body for RESOURCE_EXHAUSTED
        // Check X-RateLimit-Reset for exact reset time
        // Check if it's an RPD exhaustion (needs to wait until midnight PT)
        if strings.Contains(string(body), "RESOURCE_EXHAUSTED") {
            info.GeminiErrorType = "RESOURCE_EXHAUSTED"
        }
        if reset := headers.Get("X-RateLimit-Reset"); reset != "" {
            if ts, err := strconv.ParseInt(reset, 10, 64); err == nil {
                info.ResetTimestamp = ts
                nowMs := time.Now().UnixMilli()
                resetMs := ts * 1000
                if resetMs > nowMs {
                    info.RetryAfterMs = resetMs - nowMs
                }
            }
        }
        // If reset time is more than 60 minutes away, it's an RPD exhaustion
        if info.RetryAfterMs > 3600*1000 {
            info.IsRPDExhausted = true
        }

    case "cerebras":
        // Cerebras: x-ratelimit-reset-tokens-minute is fractional seconds
        if tokenReset := headers.Get("x-ratelimit-reset-tokens-minute"); tokenReset != "" {
            if secs, err := strconv.ParseFloat(tokenReset, 64); err == nil {
                info.CerebrasTokResetMs = int64(secs * 1000)
            }
        }
        if rem := headers.Get("x-ratelimit-remaining-requests"); rem != "" {
            if n, err := strconv.Atoi(rem); err == nil {
                info.RemainingRequests = n
            }
        }
        // Use token reset time if longer than standard Retry-After
        if info.CerebrasTokResetMs > info.RetryAfterMs {
            info.RetryAfterMs = info.CerebrasTokResetMs
        }
    }

    return info
}

// WaitDuration returns how long to wait before retrying this provider
func (r RateLimitInfo) WaitDuration() time.Duration {
    if r.IsRPDExhausted {
        // Wait until midnight Pacific Time
        return timeUntilMidnightPT()
    }
    if r.RetryAfterMs > 0 {
        // Add 10% jitter
        jitter := time.Duration(rand.Int63n(r.RetryAfterMs/10)) * time.Millisecond
        return time.Duration(r.RetryAfterMs)*time.Millisecond + jitter
    }
    // Default exponential backoff if no header found
    return defaultBackoff()
}
```

---

## Native Provider Fallback Integration

**This is the key architectural insight.** Instead of always doing cross-provider fallback
at the proxy level, exploit native provider fallback where it exists. This reduces latency
and leverages the provider's own provider-selection intelligence.

```go
// pkg/proxy/native_fallback.go

// NativeFallback attempts to use the provider's own fallback mechanism
// before falling through to cross-provider fallback.

// OpenRouter native fallback: pass a `models` array
// Returns a modified request body for OpenRouter
func BuildOpenRouterRequest(req Request, freeModels []string) map[string]any {
    body := req.ToMap()
    body["model"] = freeModels[0]
    body["models"] = freeModels // OpenRouter tries these in order
    return body
}

// OpenRouter free router: use "openrouter/free" as ultimate fallback
// OpenRouter will pick any available free model
func OpenRouterFreeRouterRequest(req Request) map[string]any {
    body := req.ToMap()
    body["model"] = "openrouter/free"
    return body
}

// Groq service tier: maximize capacity by asking for "auto" tier
func BuildGroqRequest(req Request) map[string]any {
    body := req.ToMap()
    body["service_tier"] = "auto"
    return body
}

// The fallback chain logic:
// 1. If OpenRouter key available: build OpenRouter request with models[] array
//    → OpenRouter handles intra-provider fallback automatically
//    → Only fall through to next provider if OpenRouter itself fails (not if a model fails)
// 2. Try Groq with service_tier: auto
// 3. Try Gemini, respecting all 4 rate limit dimensions
// 4. Try remaining providers
// 5. Final fallback: OpenRouter with model: "openrouter/free"
```

**Cerebras also reads back rate headers on every response** (not just 429). Read them
proactively to pre-empt rate limits:

```go
// After every Cerebras response (success or failure):
info := ExtractRateLimitInfo("cerebras", resp.Header, nil)
if info.RemainingRequests <= 2 {
    // Pre-emptively slow down — we're about to hit the limit
    rateLimiter.PreemptiveSlowdown("cerebras", info.WaitDuration())
}
```

---

## Three-Layer Model Architecture (unchanged from previous version)

```
Layer 1: PROVIDER CONFIG     — how to talk to each provider (URL, auth, headers)
                               config.yaml — human-maintained

Layer 2: LIVE CATALOG        — what models exist right now, which are free
                               ~/.picoclaw-free-llm/catalog.json
                               Updated by: picoclaw-scan

Layer 3: ENRICHED MAP        — models with tier, param count, cross-provider correlation
                               ~/.picoclaw-free-llm/enriched.json
                               Generated from catalog by classifier
```

---

## Agnostic Parameter Classifier

```go
// pkg/models/classifier.go

// ExtractParams parses parameter count from raw model IDs.
// Works on strings it has never seen before. No model name hardcoding.
func ExtractParams(modelID string) ParamScale {
    lower := strings.ToLower(modelID)

    // Pattern 1: NxMb — MoE (e.g. "mixtral-8x7b")
    if moe := moeRegex.FindStringSubmatch(lower); len(moe) == 3 {
        n, _ := strconv.ParseFloat(moe[1], 64)
        m, _ := strconv.ParseFloat(moe[2], 64)
        effective := n * m * 0.23 // empirical active param ratio for MoE
        return ParamScale{Billions: m, Effective: effective, IsEstimated: false}
    }

    // Pattern 2: explicit Nb / NB (e.g. "llama-3.1-8b", "GPT-oss-120B", "SmolLM2-1.7B")
    if explicit := paramRegex.FindStringSubmatch(lower); len(explicit) >= 2 {
        b, _ := strconv.ParseFloat(explicit[1], 64)
        return ParamScale{Billions: b, Effective: b, IsEstimated: false}
    }

    // Pattern 3: keyword fallback (only when no number found)
    // These are rough estimates — mark IsEstimated: true
    keywords := map[string]float64{
        "nano":  0.5, "tiny": 1.0, "mini": 3.0,
        "small": 7.0, "medium": 13.0, "large": 70.0, "xl": 70.0, "xxl": 180.0,
    }
    for kw, b := range keywords {
        if strings.Contains(lower, kw) {
            return ParamScale{Billions: b, Effective: b, IsEstimated: true}
        }
    }

    return ParamScale{IsEstimated: true} // unknown — classify as Balanced to be safe
}

// ClassifyTier uses ONLY numeric thresholds — no model name matching
func ClassifyTier(params ParamScale) ModelTier {
    effective := params.Effective
    if effective == 0 { effective = params.Billions }
    switch {
    case effective == 0:    return TierBalanced   // unknown — default to middle
    case effective < 3:     return TierTiny       // <3B
    case effective < 14:    return TierSmall      // 3-13B
    case effective < 80:    return TierBalanced   // 14-79B
    default:                return TierPerformance // 80B+
    }
}
```

---

## HuggingFace Special: 503 Loading Handler

```go
// pkg/providers/huggingface.go

func (p *HuggingFaceProvider) Call(ctx context.Context, req Request) (*Response, error) {
    resp, err := p.httpClient.Do(buildRequest(req))
    if err != nil { return nil, err }

    if resp.StatusCode == 503 {
        body, _ := io.ReadAll(resp.Body)
        // HuggingFace-specific: model is cold-starting
        // {"error":"Model is currently loading","estimated_time":20.0}
        if strings.Contains(string(body), "loading") || strings.Contains(string(body), "currently loading") {
            var hfErr struct {
                EstimatedTime float64 `json:"estimated_time"`
            }
            json.Unmarshal(body, &hfErr)
            waitMs := int64(hfErr.EstimatedTime * 1000)
            if waitMs <= 0 || waitMs > 60000 { waitMs = 30000 }
            // THIS IS NOT A FAILURE — wait for the model to load, then retry SAME provider
            log.Infof("HuggingFace model loading, waiting %dms...", waitMs)
            time.Sleep(time.Duration(waitMs) * time.Millisecond)
            return p.Call(ctx, req) // retry same provider
        }
    }

    // All other 5xx = real failure, fall through to next provider
    // ...
}
```

---

## Gemini Multi-Dimensional Rate Tracker

```go
// pkg/ratelimit/gemini.go
// Gemini requires tracking 4 independent dimensions per model (not per provider)

type GeminiModelLimits struct {
    RPM, TPM, RPD int
}

type GeminiTracker struct {
    mu          sync.Mutex
    modelLimits map[string]GeminiModelLimits // per model, not global
    rpmCounts   map[string]int
    tpmCounts   map[string]int
    rpdCounts   map[string]int
    dayStart    time.Time  // midnight PT
}

// CanRequest checks all 4 dimensions for the specific Gemini model being used
func (t *GeminiTracker) CanRequest(modelID string, estimatedTokens int) bool {
    t.mu.Lock()
    defer t.mu.Unlock()
    t.maybeResetDay()
    limits, ok := t.modelLimits[modelID]
    if !ok { return true } // unknown model — allow (will fail naturally)
    return t.rpmCounts[modelID] < limits.RPM &&
           t.tpmCounts[modelID]+estimatedTokens < limits.TPM &&
           t.rpdCounts[modelID] < limits.RPD
}
```

---

## Strategies (13 total — same as before, new additions noted)

All strategies from the previous prompt version are retained. Add these refinements:

**`StrategyAdaptive` OpenRouter enhancement:**
When the adaptive strategy selects OpenRouter, build the request with `models[]` array
containing the top 5 free models in priority order. This makes OpenRouter itself handle
the intra-free-model fallback, reducing round trips.

**`StrategyVolume` Gemini multi-model awareness:**
Gemini Flash-Lite has the highest RPD (1000/day) among Gemini models. When Gemini Flash
hits its RPD limit, rotate to Flash-Lite for the remainder of the day before falling
to other providers.

**`StrategySmall` Cerebras pre-emption:**
After each successful Cerebras response, read the `x-ratelimit-remaining-requests` header.
If remaining < 5, pre-emptively route next request to Groq 8B instead of waiting for the
429.

---

## `picoclaw-scan` Provider-Aware Discovery

The scanner must use provider-specific discovery logic, not a generic `/v1/models` call:

```go
// pkg/scan/scanner.go

type ProviderScanner interface {
    ScanFreeModels(ctx context.Context, cfg ProviderConfig) ([]CatalogEntry, error)
}

// Each provider gets its own scanner implementation:

// OpenRouterScanner: Uses pricing.prompt == "0" field to detect free models
// Also accepts the :free suffix as secondary confirmation
// Reads x-total-count header to know how many pages to fetch (OpenRouter paginates)

// GroqScanner: All models are free — just lists models and marks all as free
// Separately fetches per-model rate limits from the rate limits docs page (scrape or hardcode)

// GeminiScanner: Uses /v1beta/models endpoint, not OpenAI compat endpoint
// Filters by supportedGenerationMethods containing "generateContent"
// Marks free models as those accessible without billing enabled

// HuggingFaceScanner: Uses HF Hub API (NOT /v1/models) with filters:
//   task=text-generation, inference=warm, NOT gated
// GET https://huggingface.co/api/models?pipeline_tag=text-generation&inference=warm&limit=50
// This returns a manageable list of warm, non-gated models

// CerebrasScanner: Standard /v1/models, all free (subject to limits)
// Read Cerebras docs to get per-model RPS limits and add to catalog

// MistralScanner: Standard /v1/models, cross-reference with AI Studio free model list
// Some Mistral models are paid-only even with a free API key

// CohereScanner: Standard /v1/models, mark Trial key models as free
// Only "command-r" and related models are on the free Trial key
```

---

## Staleness-Aware Catalog Update Logic

```go
// When to trigger which update:

// Startup check:
//   if catalog.json missing or > 24h old → run picoclaw-scan update
//   if enriched.json missing or catalog newer than enriched → run picoclaw-scan enrich

// Rate limit events:
//   If a provider returns 429 on a model that the catalog marks as free:
//     → mark that model's catalog entry as "needs_reverification: true"
//     → next scan will probe it specifically

// LLM refresh trigger:
//   Weekly (configurable). Uses refresh-prompt.txt template.
//   The refresh prompt must tell the LLM to:
//     1. Search for each provider's current free tier page
//     2. Check for any new providers
//     3. Return structured JSON diff (not full catalog — just what changed)
//   Merge the diff into existing catalog conservatively

// The refresh result is ALWAYS a diff, never a full replacement:
//   { "changed": [...], "added": [...], "removed_confidence": [...] }
//   removed_confidence entries are flagged for human review, not auto-deleted
```

---

## `config.example.yaml` with Provider-Specific Notes

```yaml
# picoclaw-free-llm configuration
# IMPORTANT: Free tier limits change without notice.
# Run: picoclaw-scan refresh-llm  to update with current limits.
# Live limits tracker: https://github.com/cheahjs/free-llm-api-resources

proxy:
  port: 8080
  auth_token: ""
  cache_ttl: 300
  log_level: "info"
  strategy: "adaptive"

  strategy_overrides:
    performance:   { max_tokens_cap: 2048 }
    small:         { max_tokens_cap: 256 }
    tiny:          { max_tokens_cap: 128 }
    speed:         { max_tokens_cap: 512 }
    parallel:      { fan_out: 3 }

  similar:
    model_family: "llama-3.1-70b"

fallback:
  retry_on_429: true
  retry_on_5xx: true
  max_attempts: 5
  # Cerebras-specific: add spacing between requests to avoid RPS spikes
  cerebras_request_spacing_ms: 100

refresh:
  model: "groq/llama-3.3-70b-versatile"
  provider: "groq"
  schedule: "weekly"
  prompt_file: "configs/refresh-prompt.txt"
  web_search: true
  output_merge: "conservative"

catalog:
  path: "~/.picoclaw-free-llm/catalog.json"
  enriched_path: "~/.picoclaw-free-llm/enriched.json"
  max_age_hours: 24
  auto_scan_on_start: false

providers:

  - id: openrouter
    name: "OpenRouter"
    base_url: "https://openrouter.ai/api/v1"
    api_key_env: "OPENROUTER_API_KEY"
    enabled: true
    priority: 1   # First because it has native multi-model fallback
    timeout: "30s"
    extra_headers:
      HTTP-Referer: "https://github.com/your-username/picoclaw-free-llm"
      X-Title: "PicoClaw Free LLM"
    discovery:
      models_endpoint: "/v1/models"
      # OpenRouter: detect free via pricing field, not just :free suffix
      free_detect_field: "pricing.prompt"
      free_detect_value: "0"
      free_markers: [":free"]
      probe_for_free: false  # pricing field is reliable, no probe needed
    native_fallback:
      enabled: true  # Use OpenRouter's models[] array for intra-provider fallback
      free_router_model: "openrouter/free"  # ultimate fallback model ID

  - id: groq
    name: "Groq"
    base_url: "https://api.groq.com/openai/v1"
    api_key_env: "GROQ_API_KEY"
    enabled: true
    priority: 2
    timeout: "15s"
    native_features:
      service_tier: "auto"  # Always send service_tier: auto
    discovery:
      models_endpoint: "/openai/v1/models"
      all_free: true  # All listed models are free on Groq
    rate_limit_notes: "Per-model limits, not global. Track per model."

  - id: gemini
    name: "Google AI Studio"
    base_url: "https://generativelanguage.googleapis.com/v1beta/openai/"
    auth_header: "x-goog-api-key"
    auth_prefix: ""  # NO 'Bearer ' prefix
    api_key_env: "GEMINI_API_KEY"
    enabled: true
    priority: 3
    timeout: "30s"
    discovery:
      models_endpoint: "https://generativelanguage.googleapis.com/v1beta/models"
      # NOTE: uses different base URL for discovery than for inference
    rate_limit_notes: "4 dimensions: RPM, TPM, RPD, IPM. Per PROJECT not per key. RPD resets midnight PT."
    preferred_volume_model: "gemini-flash-lite"  # highest RPD (1000/day as of Dec 2025)

  - id: mistral
    name: "Mistral AI"
    base_url: "https://api.mistral.ai/v1"
    api_key_env: "MISTRAL_API_KEY"
    enabled: false
    priority: 4
    timeout: "30s"
    rate_limit_notes: "Workspace-level limits. Only 2 RPM free tier. Check Retry-After header."

  - id: cerebras
    name: "Cerebras"
    base_url: "https://api.cerebras.ai/v1"
    api_key_env: "CEREBRAS_API_KEY"
    enabled: false
    priority: 5
    timeout: "15s"
    rate_limit_notes: "RPS limits (not RPM). Read x-ratelimit-reset-tokens-minute. Space requests 100ms apart."

  - id: together
    name: "Together AI"
    base_url: "https://api.together.xyz/v1"
    api_key_env: "TOGETHER_API_KEY"
    enabled: false
    priority: 6
    timeout: "30s"
    tier_type: "credit"  # $25 free credit, not traditional free tier

  - id: deepseek
    name: "DeepSeek"
    base_url: "https://api.deepseek.com/v1"
    api_key_env: "DEEPSEEK_API_KEY"
    enabled: false
    priority: 7
    timeout: "60s"
    tier_type: "credit"  # ~$5 free credit

  - id: cohere
    name: "Cohere"
    base_url: "https://api.cohere.ai/v1"
    api_key_env: "COHERE_API_KEY"
    enabled: false
    priority: 8
    timeout: "30s"
    extra_headers:
      Cohere-Version: "2022-12-06"  # Required header — check docs for latest version
    rate_limit_notes: "1000 req/MONTH (not per day). Monthly reset, not daily."

  - id: nvidia-nim
    name: "NVIDIA NIM"
    base_url: "https://integrate.api.nvidia.com/v1"
    api_key_env: "NVIDIA_API_KEY"
    enabled: false
    priority: 9
    timeout: "30s"
    tier_type: "credit"
    rate_limit_notes: "40 RPM hard limit regardless of credits."

  - id: huggingface
    name: "Hugging Face Inference"
    base_url: "https://api-inference.huggingface.co/v1"
    api_key_env: "HF_API_KEY"
    enabled: false
    priority: 10
    timeout: "120s"  # Long timeout: cold starts can take 60s
    rate_limit_notes: "300 calls/hour/token. 503+loading = cold start, wait then retry SAME provider."
    discovery:
      skip_models_endpoint: true  # /v1/models has 100k+ models — use HF Hub API instead
      hub_api_filters: "pipeline_tag=text-generation&inference=warm&gated=false&limit=50"

  - id: ollama-local
    name: "Local Ollama"
    base_url: "http://localhost:11434/v1"
    api_key: "ollama"
    enabled: false
    priority: 99
    timeout: "120s"
    discovery:
      all_free: true
```

---

## Repo Structure

```
picoclaw-free-llm/
├── README.md
├── go.mod
├── go.sum
├── Makefile
│
├── cmd/
│   ├── proxy/main.go               # picoclaw-proxy binary
│   ├── scan/main.go                # picoclaw-scan binary
│   └── bench/main.go               # picoclaw-bench binary
│
├── pkg/
│   ├── config/
│   │   ├── config.go               # Config struct, Load(), Watch()
│   │   └── provider.go             # ProviderConfig, NativeFeatures, DiscoveryConfig
│   │
│   ├── catalog/
│   │   ├── catalog.go              # CatalogEntry, Catalog structs
│   │   ├── store.go                # Read/write catalog + enriched JSON
│   │   └── staleness.go            # Age check, reverification flags
│   │
│   ├── scan/
│   │   ├── scanner.go              # ProviderScanner interface + dispatcher
│   │   ├── openrouter.go           # OpenRouter pricing-field discovery
│   │   ├── groq.go                 # Groq all-free discovery
│   │   ├── gemini.go               # Gemini Hub API discovery
│   │   ├── huggingface.go          # HF Hub API discovery (NOT /v1/models)
│   │   ├── generic.go              # Generic /v1/models for other providers
│   │   ├── prober.go               # Test call for uncertain free status
│   │   ├── llm_refresh.go          # LLM-powered diff refresh
│   │   └── correlator.go           # Cross-provider family correlation
│   │
│   ├── models/
│   │   ├── model.go                # Types: ModelTier, Capability, ParamScale
│   │   ├── enriched.go             # EnrichedModel, ModelInstance, EnrichedCatalog
│   │   ├── classifier.go           # ExtractParams(), ClassifyTier() — no hardcoding
│   │   ├── families.go             # DetectFamily() — reads families.yaml
│   │   └── capabilities.go         # DetectCapabilities() — reads capabilities.yaml
│   │
│   ├── ratelimit/
│   │   ├── tracker.go              # Per-provider RPM/RPD/RPS counters
│   │   ├── gemini.go               # 4-dimensional Gemini-specific tracker
│   │   ├── headers.go              # ExtractRateLimitInfo() — all providers
│   │   ├── persistence.go          # usage.json — survives proxy restarts
│   │   └── backoff.go              # Exponential backoff + jitter
│   │
│   ├── reliability/
│   │   ├── tracker.go              # EMA success rate per provider
│   │   └── persistence.go          # reliability.json
│   │
│   ├── strategy/
│   │   ├── strategy.go             # Strategy interface
│   │   ├── performance.go
│   │   ├── speed.go
│   │   ├── volume.go
│   │   ├── balanced.go
│   │   ├── small.go
│   │   ├── tiny.go
│   │   ├── coding.go
│   │   ├── long_context.go
│   │   ├── similar.go
│   │   ├── parallel.go
│   │   ├── reliable.go
│   │   ├── economical.go
│   │   ├── adaptive.go
│   │   └── registry.go
│   │
│   ├── proxy/
│   │   ├── server.go               # HTTP server + all routes
│   │   ├── fallback.go             # Fallback chain
│   │   ├── native_fallback.go      # Provider-specific native fallback builders
│   │   ├── stream.go               # SSE streaming proxy
│   │   ├── cache.go
│   │   └── middleware.go
│   │
│   └── bench/
│       ├── runner.go
│       ├── metrics.go
│       └── report.go
│
├── configs/
│   ├── config.example.yaml
│   ├── families.yaml
│   ├── capabilities.yaml
│   ├── adaptive-signals.yaml
│   ├── refresh-prompt.txt           # Editable LLM refresh prompt
│   └── picoclaw-integration.json
│
├── scripts/
│   ├── setup.sh
│   └── cron-refresh.sh
│
└── docs/
    ├── providers.md                 # Per-provider quirks, native features, gotchas
    ├── strategies.md
    ├── model-tiers.md
    ├── scanner.md
    └── benchmarks.md
```

---

## Implementation Order

1. `pkg/config/` — config structs (foundation)
2. `pkg/models/` — classifier, types (no deps, test with 30+ model name strings)
3. `pkg/catalog/` — store/load JSON
4. `pkg/ratelimit/headers.go` — unified header extractor (critical, test all providers)
5. `pkg/ratelimit/tracker.go` + `gemini.go` + persistence
6. `pkg/scan/` — each provider scanner, then prober, then correlator
7. `pkg/strategy/` — all 13 strategies
8. `pkg/proxy/native_fallback.go` — provider-specific request builders
9. `pkg/proxy/fallback.go` — fallback chain (uses native_fallback + strategies)
10. `pkg/proxy/server.go` — HTTP server
11. `pkg/bench/` — benchmark runner
12. `cmd/` — wire everything up
13. `configs/` data files
14. `scripts/setup.sh`, `Makefile`
15. `README.md` + `docs/`

---

## Mandatory Pre-Implementation Checklist

Before writing code for any provider, complete this checklist:

```
□ Fetched and read official API docs for this provider
□ Confirmed current free tier models (not relying on this prompt)
□ Noted every rate limit header the provider sends
□ Checked for any native fallback or routing features
□ Understood the auth mechanism (Bearer vs custom header vs no prefix)
□ Noted any provider-specific request body fields (service_tier, models[], etc.)
□ Checked if discovery uses /v1/models or a different endpoint
□ Noted what 429 body looks like for this provider
□ Noted any non-standard error codes (HuggingFace 503+loading, Gemini RESOURCE_EXHAUSTED)
□ Added a code comment citing the docs URL and verification date
```

---

## Code Quality

- Zero external runtime deps for proxy core (stdlib only). `cobra` + `viper` + `fsnotify` allowed.
- API keys never in logs at any level.
- SSE streaming proxied transparently.
- Config + enriched catalog hot-reloaded via file watch.
- Unit tests: classifier (30+ model names), all 13 strategy Rank() methods, all header extractors, fallback chain.
- Integration tests (`//go:build integration`): one live API call per provider.
- Binary target <10MB. Go vet + golangci-lint clean.
- `.github/workflows/ci.yml` — build + vet + unit tests on push.
- Every provider implementation file must have a comment block:
  ```go
  // Provider: OpenRouter
  // Docs: https://openrouter.ai/docs/
  // Last verified: 2026-02-28
  // Free tier: 50 req/day (1000 with $10+ balance)
  // Native features: models[] fallback array, openrouter/free router
  // Auth: Authorization: Bearer (standard)
  // Special headers: HTTP-Referer, X-Title required
  ```
