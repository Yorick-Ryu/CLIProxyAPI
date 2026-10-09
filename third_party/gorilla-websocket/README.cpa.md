# CPA client extension

Based on `github.com/gorilla/websocket` v1.5.3, retaining its BSD license and
upstream package tests. Root Go source files are copied verbatim except
`client.go`, `conn.go`, and the CONNECT fixtures in `client_server_test.go`; additions are `compression_context.go` and its tests.

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
