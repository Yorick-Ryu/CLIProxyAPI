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
