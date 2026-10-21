# Codex WebSocket TLS compatibility

Opt in for new Codex OAuth upstream connections:

```yaml
oauth:
  providers:
    codex:
      websocket-tls-profile: codex # native (default) | codex
```

`codex` uses a uTLS ClientHello based on official CLI 0.162.1/0.163.0
rustls captures: cipher/signature/group order, TLS 1.2/1.3, hybrid ML-KEM
and X25519 key shares, randomized extension order, and no ALPN. The profile
is independent of the advertised CLI version. It does not reproduce the
entire rustls TLS record layer or promise identical session resumption.
New connections explicitly use no client session cache, consistent with the
official per-connect TLS configuration. Existing WS connections remain reusable.

Certificate chain and hostname verification use system roots. Failures are
returned through existing connection error handling; no silent downgrade to
native TLS occurs. Set `native` to select the original Go TLS implementation.
Hot reload affects newly established connections; existing pooled connections
keep their negotiated TLS state. HTTP, WS payloads, compression and account
isolation are unchanged. OAuth settings do not leak into API-key configuration.

The local Gorilla extension performs the handshake **after** connecting and
establishing the proxy tunnel; direct, HTTP CONNECT and SOCKS5 routes are tested.
It does not add support for proxy schemes absent from the existing dialer.
SOCKS5 dialing now propagates cancellation through ContextDialer.

Verification: `go test ./internal/runtime/executor/helps ./internal/config
./internal/runtime/executor -run 'TestCodexWebsocketTLS'`; run the nested
`third_party/gorilla-websocket` suite separately when changing the hook.
The wire test decodes emitted TLS bytes against captured field values, and
TLS 1.2/1.3 integration tests cover certificate rejection, compressed WS
round trips on a reused connection, and proxy tunnel ordering.

The same profile enables handshake header serialization compatibility: Host,
Connection, Upgrade, Sec-WebSocket-Version and Sec-WebSocket-Key lead, followed
by lowercase business fields. Unknown fields and operator values are retained.
Only serialization changes; Gorilla still validates the response and handles
frames. The default native profile retains its original serialization.

TLS 1.2/1.3 regression tests complete real message exchanges and reconnect without
session resumption. This hardens CPA behavior; it does not extend the official
capture evidence to all successful official reconnect scenarios.
