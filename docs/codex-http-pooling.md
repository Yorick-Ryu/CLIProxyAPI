# Codex HTTP connection reuse

Reference: OpenAI Codex `17a0e6f63fad13f577ef87b36b4ea37fc06385e9`,
`codex-rs/http-client/src/route_aware_client_pool.rs`. Retain transport clients
across requests, then let mature HTTP transports own connection reuse.

Native Codex OAuth HTTPS requests to `chatgpt.com` retain the existing
`HelloChrome_Auto` uTLS handshake. ALPN selects Go's HTTP/1.1 transport or
`x/net/http2.Transport`; the negotiated first connection enters that transport's
normal dial path. No extra probe connection is discarded. Empty ALPN uses
HTTP/1.1. HTTP/2 multiplexes requests, respecting the peer's concurrent-stream
limit rather than opening extra connections solely to exceed it.

The LRU contains at most 256 account/proxy/target/TLS-policy partitions. Auth ID
and upstream account/user identity isolate credentials; token refresh preserves
reuse. Request proxy overrides and the configured TLS establishment timeout
participate in partitioning. HTTP/1.1 retains at most two idle connections per
partition. Targets use normalized host plus port. Cache keys are hashes and
are not logged. Existing SDK-provided transports, API-key traffic and other
provider paths keep their behavior.

Eviction closes idle connections immediately and checks again as outstanding
responses finish. Active inference is never interrupted by eviction. Explicit
`CloseIdleConnections` remains available. No new read/write/response deadline or
time-based idle timer is introduced. The existing configured TLS handshake
bound continues to apply only when establishing a new connection.

Mature transports handle ordinary peer closure and HTTP/2 stream cancellation.
If a later TLS dial negotiates a different HTTP protocol, that connection is
closed before sending an HTTP request and the old transport partition is
invalidated. The error is returned to the existing retry owner; the next
attempt renegotiates. The pool adds no application-level request replay.

Cookie storage and gzip response decoding wrap the transport as before.
Request payload rules, zstd framing, identity mapping and WS connection reuse
are unchanged. This change is not a measured latency or model-quality claim.

Implementation: `internal/runtime/executor/helps/codex_http_pool.go`.
Regression tests use real local TLS servers and count handshakes; they cover
HTTP/1.1 and HTTP/2 reuse, concurrent cold start, stream cancellation, active
response eviction, target/account/proxy/TLS-policy isolation, closed peers,
protocol change, idle closure and SDK overrides.

Run `go test -race ./internal/runtime/executor/helps -run 'TestCodex(HTTPPool|HTTP|Cookies)'`
and the full executor suite. Deployment must be verified separately against the
running image revision.
