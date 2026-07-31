# ADR 0007: Stable authenticated endpoint, API keys, and loopback-only model runtime

Status: accepted for local implementation; behavior awaiting DGX Spark qualification

## Decision

The manager serves the OpenAI-compatible API itself at `/v1/*` on its own
listen address and reverse-proxies (with streaming) to whichever model is
active on its loopback-only port. Model containers always publish their port
on `127.0.0.1`; the authenticated proxy is the only network path to inference.
This supersedes ADR 0006's "model port follows the manager listen interface"
for exposure — the model port no longer follows anything.

Requests to `/v1` authenticate with either:

- an API key (`Authorization: Bearer rosk_<48 hex>`), created and revoked in
  the console, stored only as a SHA-256 hash and shown exactly once; or
- an existing paired console session (used by the console playground; the
  session cookie is SameSite=Strict, which blocks cross-site use).

Manager credentials (cookies, keys, CSRF headers) are stripped before the
request reaches the model runtime.

## Why

- Clients are configured once. Switching models never changes the base URL,
  so Cursor/LiteLLM/scripts keep working across switches.
- The previously unauthenticated vLLM port is no longer reachable from the
  network at all, closing the LAN-exposure gap properly instead of relying on
  bind-address discipline.
- Resolves the endpoint-stability half of PRD §18's open question (the
  "reverse-proxy endpoint" option won).

## Related product surfaces added with this change

- `benchmark` job kind (`measure_throughput` operation): measures decode
  tokens/sec and time-to-first-token on this device via a fixed streamed
  generation; runs automatically once after a model's first activation and on
  demand. Results persist per recipe and are shown in the catalog instead of
  editorial speed claims.
- `GET /api/v1/telemetry`: inventory sample plus selected vLLM Prometheus
  series (requests running/waiting, KV-cache usage, token counters).
- `GET /api/v1/storage`: managed-disk breakdown per artifact/cache, with the
  database and configs, for the console storage view.
- `GET /api/v1/update`: latest-release check against GitHub with a 6-hour
  cache (see ADR 0008 for the full self-update design).
- Disk preflight failures now embed ranked `reclaim_candidates` so the
  console can offer the fix, not just the failure.
