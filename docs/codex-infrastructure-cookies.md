# Codex infrastructure cookies

Reference: OpenAI Codex `17a0e6f63fad13f577ef87b36b4ea37fc06385e9`,
`codex-rs/http-client/src/chatgpt_cloudflare_cookies.rs` and
`codex-rs/websocket-client/src/lib.rs`.

Native Codex OAuth HTTP and WebSocket transports share an in-memory cookie
store. Only the official infrastructure-name allowlist and HTTPS/WSS ChatGPT
hosts are eligible. Account/session cookies are rejected. The standard cookie
jar enforces domain, path, secure and expiration rules, with public-suffix
validation. Successful and failed HTTP/WS handshake responses can update it.
Explicit outgoing Cookie headers take precedence, including an empty header.
This does not forward arbitrary incoming client cookies.

CPA partitions the store by auth identity, upstream account/user identifiers,
and effective proxy route, including execution overrides and WS environment
proxy inheritance. Tokens and cookie values are not used as cache keys or
written to disk. Token refresh keeps the partition; changing identity or proxy
selects another partition. A proxy endpoint that silently rotates its actual
IP cannot be distinguished by this mechanism.

There are at most 256 partitions, evicted after 24 hours idle or by oldest use
at capacity. Each admits at most 64 domain/path/name slots and rejects Set-Cookie
lines over 4096 bytes. Slot reservations persist until explicit deletion or
partition eviction; cookie expiry is independently enforced by the jar.
Storage is process-local and disappears on restart. No new configuration or
management endpoint is introduced. HTTP connection pooling is unchanged.

Implementation: `internal/runtime/executor/helps/codex_cookies.go`.
Checks: `go test -race ./internal/runtime/executor/helps -run 'TestCodex(Cookies|HTTP)'`;
full executor tests also exercise the integration call sites. Compare the
running image revision with the checkout before assuming this behavior is live.
