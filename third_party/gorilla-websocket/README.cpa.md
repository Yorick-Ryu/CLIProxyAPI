# CPA client extension

Based on `github.com/gorilla/websocket` v1.5.3, retaining its BSD license and
upstream package tests. Root Go source files are copied verbatim except
`client.go`, `conn.go`, and the CONNECT fixtures / handler cleanup in `client_server_test.go`; additions are `compression_context.go` and its tests.

`Dialer.EnableContextTakeover` is opt-in and only enabled for Codex upstream
connections. It offers `permessage-deflate; client_max_window_bits`, honors
server-selected reset modes and 8–15 bit windows, retains per-connection
compression history, and drains partially consumed messages before advancing.
Prepared messages use the connection compressor in this mode. Default clients
and the server upgrader retain upstream behavior.

The small-window encoder uses the root project's existing
`github.com/klauspost/compress` v1.17.4 dependency. Run `go test -race ./...` in
this directory separately; root `go test ./...` does not traverse nested modules.

When updating Gorilla, rebase these changes and rerun both suites. Do not replace
the extension offer alone without the corresponding encoder/decoder support.

`Dialer.TLSHandshakeContext` is an opt-in client hook for verified TLS on an
already-connected transport, after any CONNECT tunnel. It is skipped for
plain WS and when `NetDialTLSContext` is supplied. On failure DialContext
closes the original transport. Codex uses this hook for its optional uTLS
profile; root helper tests exercise real TLS and proxy connections.

`Dialer.WriteHandshake` is an opt-in request serialization hook. Codex uses it to
adjust header case/order without replacing Gorilla response validation or frame
handling. With no hook, the original `http.Request.Write` path remains active.

The test server joins hijacked WebSocket handlers during cleanup so they cannot
log through `testing.T` after their test has ended. This changes test fixtures
only; the race checks remain enabled.
