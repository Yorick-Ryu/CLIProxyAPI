# Codex HTTP TLS profiles

```yaml
oauth:
  providers:
    codex:
      http-tls-profile: linux # chrome (default) | linux
```

`chrome` preserves the existing Chrome/uTLS HTTP behavior. `linux` selects the
initial ClientHello captured from official Linux musl Codex CLI 0.162.1/0.163.0
without CODEX_CA_CERTIFICATE / SSL_CERT_FILE. The source baseline is the native
TLS path, not the optional rustls path. Its ordered cipher/extension/group and
signature lists, TLS 1.2/1.3 versions and ML-KEM/X25519 shares are reproduced.
It offers no ALPN, so HTTP uses HTTP/1.1 even if the server also supports H2.
No H2 SETTINGS tuning is applied. This may change concurrency/performance versus
multiplexed H2; validate representative traffic before deployment.

This is ClientHello compatibility, not an OpenSSL replacement. Legacy algorithms
advertised by the captured client are not all implemented by uTLS; unusual server
selections may fail instead of completing a native OpenSSL handshake. TLS record behavior is not claimed identical.
Certificate chain and hostname verification remain enabled; no automatic fallback
to another profile or weaker certificate policy occurs.

The setting affects the scoped Codex OAuth ChatGPT HTTP transport only. Existing
account/proxy/host isolation, proxy dialing, cookies, request compression and SDK
transport overrides remain in effect. API-key credentials and other providers do
not inherit this profile. WS uses its separate websocket-tls-profile setting.

The connection pool includes the profile in its key, so changing profiles cannot
reuse a connection established with the previous profile. In-flight requests keep
their existing transport. The default UA now uses Linux TUI identity
(Ubuntu 24.4.0; x86_64; xterm-256color), independently of TLS mode. Explicit UA
configuration and persisted account identity headers still
apply; this is a fixed default identity, not runtime OS detection.

Tests: TestCodexLinuxHTTPWireHello, TestCodexLinuxHTTPPooling,
TestCodexLinuxHTTPProfileSelection, TestCodexHTTPTLSConfig,
TestCodexDefaultLinuxTUIIdentity. The wire test decodes emitted TLS bytes against
capture values; integration tests cover TLS 1.2/1.3, HTTP/1.1 and connection reuse.

The Linux profile also serializes HTTP/1.1 field names in lowercase. The
Responses template follows the observed OpenAI-provider TUI sequence: `version`,
beta/turn-state fields, window and turn metadata, compatibility projections,
responses-lite, routing hint, request/session IDs, `accept`,
`content-encoding`, `content-type`, authorization/account, originator,
user-agent, cookie, then host/length framing. Unknown OAuth/operator headers
retain their relative order. Values, repeated fields and body framing are
preserved; this is not a universal ordering template for every CLI request.

New Linux TLS connections explicitly use no client session cache, matching the
observed native Linux behavior. Ticket extensions remain advertised. Successful
HTTP/1.1 connections still reuse the existing account-scoped pool. TLS 1.2/1.3
tests cover successful responses followed by fresh, non-resumed connections.

The Linux switch also applies to v8 management api-call GET probes for Codex
OAuth accounts at chatgpt.com/backend-api/wham/usage, backend-api/subscriptions
and backend-api/wham/rate-limit-reset-credits. Explicit request proxies, account
proxies and global proxies retain their precedence, including direct overrides.
The deprecated v0 operation and other providers/targets retain existing behavior.

Official CLI backend-client quota/credits reads use its HTTP client, not its WS
connector; Linux native TLS is therefore the selected baseline. Subscriptions
is an OMC query, not established here as a corresponding CLI operation. Query
header values are preserved. Names follow observed CLI query groups (user-agent, authorization,
chatgpt-account-id, accept), with unknown fields retained and framing last,
but no Responses session-header template is injected or universal official query
header order claimed. These calls do not inherit Responses body transformations,
cookie storage or UA overrides. Their per-call clients release idle connections.

### Image requests

With `oauth.providers.codex.http-tls-profile: linux`, native Codex OAuth image
requests use the same account/proxy-partitioned Linux transport as Responses,
including connection reuse and suppression of automatic `Accept-Encoding`.
This covers direct Images and Responses-based image execution, streaming and
nonstreaming. API-key/SIWC credentials and the disabled profile keep their
previous transport.

Direct `/images/generations` and `/images/edits` use a separate observed Linux
header-order policy, selected per request even on reused connections. Their
Accept default is `*/*`; Version follows the effective Codex UA unless explicitly
pinned, and the image turn ID follows mapped turn metadata or an account-scoped
client ID, with UUIDv7 generation when absent. Client Accept and account/model
header overrides remain supported; persisted account identity headers retain precedence.
No extra payload mutations or TLS session cache are introduced.

### Standalone Alpha Search

With `oauth.providers.codex.http-tls-profile: linux`, native OAuth POST
`/alpha/search` uses the observed Linux TUI header groups and defaults Accept
to `*/*`. Its effective Originator, UA and Version follow the existing identity,
cloaking, administrator-header and persisted account identity settings. API-key/SIWC
credentials and a disabled Linux profile retain their previous behavior.

Forward existing turn metadata without applying the Responses struct layout:
keep the extension map's field order and use ASCII JSON escapes. Search body
`id`, metadata session/thread/turn references and existing history turn IDs use
the same account namespace as Responses, preserving UUIDv7 timestamps.
Do not synthesize absent metadata or Responses-only headers. Request preparation
is idempotent across manager preparation/execution; a new account rebuilds from
the original request. Any context-provided payload finalizer runs after these
semantic changes, once per prepared attempt.

Verified with actual Linux interactive TUI 0.162.1 and 0.163.0, including
`Originator: codex-tui`, `Accept: */*` and native metadata/header layouts. The
0.163.0 direct binary used `--no-daemon` because no complete runtime package was
installed in the isolated home. This remains an interactive TUI with an embedded
app-server, distinct from the earlier manually initialized app-server fixture.
