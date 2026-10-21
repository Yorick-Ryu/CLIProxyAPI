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
selections may fail instead of completing a native OpenSSL handshake. Session
resumption, TLS record behavior and HTTP header ordering are not claimed identical.
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
configuration and the existing optional startup version synchronization still
apply; this is a fixed default identity, not runtime OS detection.

Tests: TestCodexLinuxHTTPWireHello, TestCodexLinuxHTTPPooling,
TestCodexLinuxHTTPProfileSelection, TestCodexHTTPTLSConfig,
TestCodexDefaultLinuxTUIIdentity. The wire test decodes emitted TLS bytes against
capture values; integration tests cover TLS 1.2/1.3, HTTP/1.1 and connection reuse.
