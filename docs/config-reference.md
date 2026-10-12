# Configuration reference (verified against code, round 7 — 2026-10-12)

Every key below was checked against the source at commit `8cffcd2`
(`canonical-model-aliases`). Defaults are the viper defaults registered in
`pkg/config/config.go`. "Hot reload" = the running proxy picks up a config
file change (fsnotify watch, `cmd/proxy/main.go:175`) without a restart.

## Slot gate (RPM limiter)

| Key | YAML position | Default | Read at | Hot reload |
|---|---|---|---|---|
| `providers[].rpm_limit` | per provider | 0 (gate off) | `pkg/config/provider.go:75`; gate `pkg/proxy/slots.go:179-193` | yes — read per request |
| `providers[].rpm_limit_overrides` | per provider, map | — | `pkg/config/provider.go:82`; `RPMFor` `provider.go:87-92` | yes — read per request |

The limiter counts slots **per model**: keys are `"<provider>/<model>"` and
each model id owns its own slot pool (`slots.go:174`, test
`slots_429_test.go:78,110`). A reserved slot is never released — a failed
attempt consumes the real upstream quota (by design, NG).

## Slot queue

| Key | YAML position | Default | Read at | Hot reload |
|---|---|---|---|---|
| `fallback.queue_wait_seconds` | fallback | 90 (0 = queue off) | `pkg/config/config.go:446`; `pkg/proxy/fallback.go:162-167`, `pkg/proxy/queue.go:19` | yes |
| `fallback.queue_max_waiters` | fallback | 64 | `pkg/config/config.go:447,266`; `pkg/proxy/fallback.go:171-175` | yes |

Queue semantics (`pkg/proxy/queue.go`): FIFO per requested model group; wake
timer armed at the earliest next_available; a client that disconnects leaves
the queue immediately holding no slot; overflow → 503 + Retry-After.

## Write deadlines (task S)

| Key | YAML position | Default | Read at | Hot reload |
|---|---|---|---|---|
| `proxy.write_idle_timeout_seconds` | proxy | 30 | `pkg/config/config.go:429`; `pkg/proxy/writedeadline.go:104-110` | yes |
| `proxy.max_request_seconds` | proxy | 0 = auto (queue_wait + attempt + stream + idle) | `pkg/config/config.go:430`; `writedeadline.go:114` | yes |
| `proxy.write_timeout_seconds` | proxy | 120 — RETIRED no-op | `pkg/config/config.go:428` | — |

The server-level `http.Server.WriteTimeout` is disabled; every successful
write/flush refreshes the deadline to `now + write_idle_timeout_seconds`,
clamped to the request ceiling (`writedeadline.go:22-27`).

## Streaming caps

| Key | YAML position | Default | Read at | Hot reload |
|---|---|---|---|---|
| `fallback.stream_timeout_seconds` | fallback | 0 = inherit `provider_timeout_seconds` (120) | `pkg/config/config.go:439,197-201`; `pkg/proxy/server.go:189-198` | yes |
| `fallback.stream_idle_timeout_seconds` | fallback | 30 | `pkg/config/config.go:445` | yes |
| `fallback.first_token_timeout_seconds` | fallback | 30 | `pkg/config/config.go:444`; `pkg/proxy/stream_chain.go:437` | yes |
| `fallback.first_token_timeouts` | fallback, map `"provider"` / `"provider/model"` → seconds | — | `pkg/proxy/stream_chain.go:436-438` | yes |

`stream_timeout_seconds` caps the WHOLE streaming attempt (Go
`http.Client.Timeout`): a stream flowing longer than this is cut with an
in-band SSE error event + `[DONE]` (`stream_chain.go:694-706`) — never a
silent fake success. Raise it for long generations.

## Unknown-400 policy (round 8)

| Key | YAML position | Default | Read at | Hot reload |
|---|---|---|---|---|
| `fallback.unknown_400_policy` | fallback | `cross_model_confirm` | `pkg/config/config.go:253`; `pkg/proxy/classify.go:247` (`classifyBadRequest`), `pkg/proxy/demote.go` (`failureDecisionWalk`) | yes |
| `fallback.unknown_400_confirm_n` | fallback | 2 | `pkg/config/config.go:260`; `pkg/proxy/demote.go` (`failureDecisionWalk`) | yes |
| `fallback.unknown_400_demotion_seconds` | fallback | 600 | `pkg/config/config.go:269`; `pkg/proxy/demote.go` (`demotionTTL`, `filterDemotedEntries`, `filterDemotedUpstreams`) | yes |

How an UNRECOGNIZED 400 (no content-policy / context-length / modality
marker matched) is handled:

- `cross_model_confirm` (the default, and the only recognized value): the
  walk moves to the NEXT candidate — a different upstream model — instead
  of surfacing the 400. Only when `unknown_400_confirm_n` DISTINCT models
  have answered the same unrecognized 400 does the walk fail-fast and
  surface the real upstream error verbatim. Wired at every multi-candidate
  walk: the ranked walk, the direct route's catalog-entry loop, and the
  alias route's upstream loop (pool-alias traffic). The openrouter
  `models[]` step and the `openrouter/free` ultimate fallback keep the
  legacy fail-fast (a multi-model request cannot confirm across models).
- Any other value — empty, `fail_fast`, a typo — keeps the historical
  behavior: the first unrecognized 400 surfaces immediately. Values fail
  closed; there is no normalize/validate pass in `pkg/config`.
- `unknown_400_demotion_seconds` additionally parks an unrecognized-400
  model out of EVERY walk (cross-request) for this many seconds. The
  demotion registry is in-memory, bounded at 64 entries; the OLDEST
  demotion is handed back FIFO when full, entries re-enter ranking when
  the TTL lapses (lazy expiry), and when every candidate is demoted they
  are handed back (last chance) rather than starving the request.
  `0` disables cross-request demotion.

Each policy-rerouted model is also demoted (same registry), so the next
request skips it without waiting for a fresh confirm cycle.

## Slow-TTFT circuit breaker + payload trimming (round 10)

| Key | YAML position | Default | Read at | Hot reload |
|---|---|---|---|---|
| `fallback.ttft_slow_threshold_seconds` | fallback | 10 | `ttftSlowThresholdFrom` `pkg/proxy/latency.go` | yes — read per observation |
| `fallback.ttft_breaker_trips` | fallback | 3 | `ttftBreakerTripsFrom` `pkg/proxy/latency.go` | yes |
| `fallback.ttft_breaker_min_samples` | fallback | 3 | `ttftBreakerMinSamplesFrom` `pkg/proxy/latency.go` | yes |
| `fallback.context_truncation.enabled` | fallback | true | `applyContextTruncation` `pkg/proxy/truncate.go` | yes |
| `fallback.context_truncation.prompt_token_ceiling` | fallback | 8000 | `applyContextTruncation` | yes |
| `fallback.context_truncation.keep_recent_turns` | fallback | 6 | `applyContextTruncation` | yes |

How it works (`pkg/proxy/latency.go`):

- **TTFT measurement** — streaming: attempt start → the commit point (first
  usable event); a TTFT-watchdog abort records a FORCED slow observation.
  Buffered: attempt start → the response headers. Observed on every route
  (strategy chain, direct, alias/pool) through
  `streamClientParams.OnTTFT` / `callProvider`.
- **Latency EMA** — per `provider/model` (α = 0.25), in-memory like the
  demotion registry. Snapshots: `GET /debug/latency`.
- **Soft deprioritization** — candidates whose EMA TTFT ≥
  `ttft_slow_threshold_seconds` with ≥ `ttft_breaker_min_samples` samples
  move to the BACK of the ranking (stable, never dropped) on every request.
- **Circuit breaker** — `ttft_breaker_trips` consecutive slow observations
  demote the model into the round-8 demotion registry (same TTL as
  `unknown_400_demotion_seconds`, same FIFO hand-off and last-chance rule);
  the next request skips it at ranking until the TTL lapses.
- **Context truncation** (`pkg/proxy/truncate.go`) — history over
  `prompt_token_ceiling` (4 chars ≈ 1 token) is sliding-windowed to every
  system message + the last `keep_recent_turns` turns; a still-oversized
  single string message is trimmed head+tail with an explicit
  `[context truncated]` marker. Tool-calling requests are never truncated.
  The capability gate still ranks the FULL prompt — only what is sent is
  trimmed.

## Persistence

| Key | YAML position | Default | Read at | Hot reload |
|---|---|---|---|---|
| `proxy.slot_persist_interval_seconds` | proxy | 10 (negative disables) | `pkg/config/config.go:431,63`; `pkg/proxy/slotpersist.go:189`; `cmd/proxy/main.go:140-144` | **no** — boot-time only |

Graceful shutdown always writes a final slots.json; the periodic save bounds
what SIGKILL/OOM loses to one interval.

## Discovery (live :free refresh, task Q)

| Key | YAML position | Default | Read at | Hot reload |
|---|---|---|---|---|
| `providers[].discovery.free_markers` | per provider | `[":free"]` | `pkg/config/provider.go:120`; `pkg/scan/unorouter.go:75-77` | scanner-side |
| `providers[].discovery.refresh` | per provider | false | `pkg/config/provider.go:133`; `pkg/proxy/model_refresh.go:22` | next tick |
| `models.model_refresh_minutes` | models | 20 (0 = off) | `pkg/config/config.go:450,159`; `model_refresh.go:50-52` | next tick |

A failed refresh keeps the existing list — groups are never cleared
(`model_refresh.go:106` + test `model_refresh_test.go:113`).

## Other knobs (unchanged defaults)

`fallback.max_attempts` 5, `retries_per_candidate` 1, `retry_after_seconds` 5,
`max_fallback_hops` 8, `total_deadline_seconds` 90 (auto-raised to
queue_wait + attempt ceiling when queueing is on — `server.go:215-236`),
`provider_timeout_seconds` 120. `fallback.retry_on_429` is a retired no-op
(`classify.go:119,169`); `retry_on_5xx` is live (`classify.go:140,188`).
There is **no** `scoring.*` or `ttft_soft/ttft_hard` key in this codebase —
per-model TTFT tuning is `fallback.first_token_timeouts`.

## UnoRouter free-tier example (production)

```yaml
providers:
  - id: unorouter
    name: "UnoRouter"
    base_url: "https://api.unorouter.com/v1"
    api_key_env: "UNOROUTER_API_KEY"
    enabled: true
    priority: 1
    timeout: "60s"
    rpm_limit: 1            # free tier: 1 request/minute PER MODEL
    # rpm_limit_overrides: {"model-id:free": 2}   # optional per-model cap
    discovery:
      free_markers: [":free"]
      refresh: true
```

With N healthy free models the pool serves ~N requests/minute; demand above
that waits in the FIFO queue (up to `queue_wait_seconds`) and then gets a
structured 503 + Retry-After — the limiter never forwards a 429-causing
burst upstream.

## Rollback guide (config layer)

```bash
# 1. Restore the previous config (adjust the backup dir).
cp -a ~/backup-r7-<TS>/config.yaml /opt/free-llm-proxy-router/config.yaml
# 2. Hot-reload fires automatically; verify:
journalctl -u free-llm-proxy -n 5 --no-pager     # expect "config hot-reloaded"
curl -s http://127.0.0.1:8080/health              # expect {"status":"ok"}
# 3. If the watcher is dead, restart instead:
sudo systemctl restart free-llm-proxy
```

Full-binary rollback: swap `/opt/free-llm-proxy-router/bin/free-llm-proxy`
back to `free-llm-proxy.old-0844c7e`, `sudo systemctl restart`, `curl /health`.

## Health checks

```bash
systemctl is-active free-llm-proxy
curl -s http://127.0.0.1:8080/health                 # {"status":"ok"}
curl -s http://127.0.0.1:8080/v1/models              # pool alias list
curl -s http://127.0.0.1:8080/debug/pool             # upstream candidates backing the alias
curl -s http://127.0.0.1:8080/debug/latency          # per-model TTFT EMA / slow streaks / breaker trips (round 10)
cat ~/.free-llm-proxy-router/slots.json              # per-model slot state (version 1)
journalctl -u free-llm-proxy --since "10 min ago" | grep -ciE "panic|error"   # expect 0
```

