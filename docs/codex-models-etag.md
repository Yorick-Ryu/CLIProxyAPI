# Suppress upstream model catalog refresh hints

When a Codex client loads a custom catalog, the upstream inference server's
`x-models-etag` can differ permanently from that catalog's `ETag`. Each hint
can then trigger another full catalog download.

```yaml
client:
  codex:
    strip-models-etag: true
```

Default: `false`. The setting applies at the client-facing Responses boundary
across providers and OAuth/API-key routes. It removes only:

- `x-models-etag` from successful Responses and compact HTTP response headers.
- Case-insensitive `x-models-etag` keys inside the `headers` object of
  `codex.response.metadata` events, over SSE and WebSocket.

Other metadata, including turn state and safety buffering, is preserved. The
catalog endpoint and its own `ETag` are unchanged. Codex's periodic discovery
continues; immediate invalidation and cache renewal via this hint stop.

The flag is sampled once per HTTP request or WebSocket turn. Hot reload affects
new requests and subsequent turns on existing sockets; an active turn keeps its
snapshot. Ordinary events reuse the already decoded event type and require no
additional JSON parsing or allocation. Only matching metadata is rewritten.

Disable the flag to restore forwarding. Remove this new configuration key before
rolling back to an older binary with strict configuration validation. No stored
data is changed. Home auth/routing payloads and management endpoints are unchanged.

Validation uses isolated HTTP/WebSocket fixtures, including config reload on
one socket. Run the focused benchmarks with:

```sh
go test ./sdk/api/handlers/openai -run '^$' -bench '^BenchmarkResponsesModelsETag$' -benchmem
```

## Hide upstream account quota and credits

```yaml
client:
  codex:
    strip-quota: true
```

Independent of `strip-models-etag`, default `false`. Enable when downstream
users have their own gateway quotas rather than owning the selected upstream
account. The client-facing Responses boundary removes Codex quota/credit
headers from HTTP/compact responses and metadata (including nested response
headers), and drops `codex.rate_limits` events over WebSocket/SSE. Existing SSE
filtering of this event remains unchanged when the switch is off.

Matching is case-insensitive: `x-codex-credits-*`, quota window suffixes
(`used-percent`, `reset-at`, `window-minutes`, `reset-after-seconds`), limit
names/status, active limit and plan type. Named/additional limit families are
covered. Unrelated `x-codex-*` headers, `Retry-After`, error codes, token usage,
model identity, turn state and safety metadata are preserved. HTTP error header
forwarding also respects the setting; internal quota observations and account
scheduling retain the original upstream data. The option does not rewrite error
messages or arbitrary response content.

HTTP success paths and WebSocket turns snapshot the flag once; hot reload takes
effect for subsequent requests/turns. HTTP errors use the current setting when
written. Ordinary text/tool delta frames require no extra parsing or allocation.
This hides misleading account information; it is not a material bandwidth saving.
Disable the flag to restore the previous behavior. Remove `strip-quota` before
rolling back to a binary that predates it; strict configuration validation in
older versions may reject the key. No stored quota or account data is changed.

```sh
go test ./sdk/api/handlers/openai -run '^$' -bench '^BenchmarkResponsesQuota$' -benchmem
```
