package helps

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/httpwire"
)

// CodexHTTPHeaderOrder follows the observed native Responses header groups.
// Additional OAuth/operator fields keep their relative order before framing
// headers. This is a compatibility policy, not a universal Hyper ordering claim.
func CodexHTTPHeaderOrder(method, target string, names []string) []string {
	// Select per request, since a pooled connection may serve both APIs.
	if u, err := url.ParseRequestURI(target); err == nil && method == http.MethodPost && strings.HasSuffix(u.Path, "/alpha/search") {
		return codexHeaderOrder(names, []string{
			"version", "x-codex-turn-metadata", "authorization", "chatgpt-account-id",
			"content-type", "originator", "user-agent", "accept", "cookie",
		}, []string{"host", "content-length", "transfer-encoding"})
	}
	if u, err := url.ParseRequestURI(target); err == nil && method == http.MethodPost &&
		(strings.HasSuffix(u.Path, "/images/generations") || strings.HasSuffix(u.Path, "/images/edits")) {
		return codexHeaderOrder(names, []string{
			"version", "x-codex-image-turn-id", "x-codex-turn-metadata", "authorization",
			"chatgpt-account-id", "content-type", "originator", "user-agent", "accept", "cookie",
		}, []string{"host", "content-length", "transfer-encoding"})
	}
	return codexHeaderOrder(names, []string{
		"x-codex-beta-features", "x-codex-window-id", "x-codex-turn-metadata",
		"x-openai-internal-codex-responses-lite", "x-client-request-id", "session-id",
		"thread-id", "accept", "content-type", "authorization", "originator", "user-agent",
	}, []string{"host", "content-length", "transfer-encoding"})
}

func codexWebsocketHeaderOrder(_, _ string, names []string) []string {
	return codexHeaderOrder(names, []string{
		"Host", "Connection", "Upgrade", "Sec-WebSocket-Version", "Sec-WebSocket-Key",
		"authorization", "user-agent", "originator", "openai-beta", "x-codex-turn-metadata",
		"x-codex-beta-features", "x-client-request-id", "session-id", "thread-id", "x-codex-window-id",
	}, []string{"sec-websocket-extensions"})
}

func codexHeaderOrder(names, first, last []string) []string {
	reserved := make(map[string]bool, len(first)+len(last))
	for _, group := range [][]string{first, last} {
		for _, name := range group {
			reserved[strings.ToLower(name)] = true
		}
	}
	order := append([]string(nil), first...)
	for _, name := range names {
		name = strings.ToLower(name)
		if !reserved[name] {
			order = append(order, name)
			reserved[name] = true
		}
	}
	return append(order, last...)
}

// WriteCodexWebsocketHandshake keeps Gorilla's generated request and response
// checks intact. Only the serialized header names/order are transformed; no
// wrappers remain on the connection to inspect subsequent WebSocket frames.
func WriteCodexWebsocketHandshake(w io.Writer, req *http.Request) error {
	var original bytes.Buffer
	if err := req.Write(&original); err != nil {
		return err
	}
	wire := httpwire.ReorderRequestHeader(original.Bytes(), codexWebsocketHeaderOrder)
	_, err := io.Copy(w, bytes.NewReader(wire))
	return err
}
