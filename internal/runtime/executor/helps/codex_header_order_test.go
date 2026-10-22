package helps

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/httpwire"
)

type codexHeaderCaptureConn struct {
	net.Conn
	data bytes.Buffer
}

func (c *codexHeaderCaptureConn) Write(p []byte) (int, error) { return c.data.Write(p) }

func TestCodexHTTPHeaderOrderPreservesBodiesAndOverrides(t *testing.T) {
	raw := &codexHeaderCaptureConn{}
	conn := httpwire.NewOrderedRequestConnWithNames(raw, CodexHTTPHeaderOrder)
	first := "POST /responses HTTP/1.1\r\nHost: test\r\nUser-Agent: admin/1\r\nContent-Length: 6\r\nX-Operator: One\r\nX-Operator: Two\r\nAuthorization: Bearer synthetic\r\nAccept: text/event-stream\r\n\r\nGET /!"
	second := "POST /compact HTTP/1.1\r\nHost: test\r\nTransfer-Encoding: chunked\r\nX-Custom: KEEP\r\n\r\n6\r\nPOST /\r\n0\r\nX-Trailer: ORIGINAL\r\n\r\n"
	third := "GET /models HTTP/1.1\r\nHost: test\r\nUser-Agent: admin/2\r\n\r\n"
	input := first + second + third
	// Split even within field names, framing and chunk boundaries.
	for i := 0; i < len(input); i++ {
		if _, err := conn.Write([]byte(input[i : i+1])); err != nil {
			t.Fatal(err)
		}
	}
	expected := "POST /responses HTTP/1.1\r\naccept: text/event-stream\r\nauthorization: Bearer synthetic\r\nuser-agent: admin/1\r\nx-operator: One\r\nx-operator: Two\r\nhost: test\r\ncontent-length: 6\r\n\r\nGET /!" +
		"POST /compact HTTP/1.1\r\nx-custom: KEEP\r\nhost: test\r\ntransfer-encoding: chunked\r\n\r\n6\r\nPOST /\r\n0\r\nX-Trailer: ORIGINAL\r\n\r\n" +
		"GET /models HTTP/1.1\r\nuser-agent: admin/2\r\nhost: test\r\n\r\n"
	if raw.data.String() != expected {
		t.Fatalf("wire mismatch\ngot %q\nwant %q", raw.data.String(), expected)
	}
}

func TestCodexWebsocketHandshakeOrderAndValidation(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		wire := make(chan string, 1)
		go func() {
			c, err := listener.Accept()
			if err != nil {
				wire <- ""
				return
			}
			defer c.Close()
			reader := bufio.NewReader(c)
			var b strings.Builder
			for {
				line, err := reader.ReadString('\n')
				if err != nil {
					wire <- b.String()
					return
				}
				b.WriteString(line)
				if line == "\r\n" {
					break
				}
			}
			wire <- b.String()
			_, _ = io.WriteString(c, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: intentionally-invalid\r\n\r\n")
		}()
		d := websocket.Dialer{HandshakeTimeout: 5 * time.Second, EnableCompression: true, EnableContextTakeover: true}
		if enabled {
			d.WriteHandshake = WriteCodexWebsocketHandshake
		}
		headers := http.Header{"User-Agent": {"admin/1"}, "Authorization": {"Bearer synthetic"}, "Originator": {"custom"}, "X-Operator": {"One", "Two"}}
		conn, resp, err := d.Dial("ws://"+listener.Addr().String()+"/responses", headers)
		if conn != nil {
			_ = conn.Close()
		}
		if resp != nil {
			_ = resp.Body.Close()
		}
		_ = listener.Close()
		if !errors.Is(err, websocket.ErrBadHandshake) {
			t.Fatalf("invalid accept bypassed validation: %v", err)
		}
		data := <-wire
		var names []string
		for _, line := range strings.Split(data, "\r\n")[1:] {
			if i := strings.IndexByte(line, ':'); i > 0 {
				names = append(names, line[:i])
			}
		}
		if enabled {
			expected := []string{"Host", "Connection", "Upgrade", "Sec-WebSocket-Version", "Sec-WebSocket-Key", "authorization", "user-agent", "originator", "x-operator", "x-operator", "sec-websocket-extensions"}
			if !reflect.DeepEqual(names, expected) {
				t.Fatalf("names %v", names)
			}
			if !strings.Contains(data, "user-agent: admin/1\r\n") || !strings.Contains(data, "x-operator: One\r\nx-operator: Two\r\n") {
				t.Fatal("operator values changed")
			}
		} else if !reflect.DeepEqual(names[:2], []string{"Host", "User-Agent"}) {
			t.Fatalf("default serializer changed: %v", names)
		}
	}
}

func TestCodexImageAndResponsesHeaderOrderOnReusedConnection(t *testing.T) {
	raw := &codexHeaderCaptureConn{}
	conn := httpwire.NewOrderedRequestConnWithNames(raw, CodexHTTPHeaderOrder)
	for _, path := range []string{"/backend-api/codex/images/generations", "/backend-api/codex/responses", "/backend-api/codex/images/edits?test=1"} {
		imageTurn := ""
		if strings.Contains(path, "/images/") {
			imageTurn = "X-Codex-Image-Turn-Id: test-turn\r\n"
		}
		input := "POST " + path + " HTTP/1.1\r\nHost: test\r\nUser-Agent: admin/1\r\nContent-Length: 2\r\nContent-Type: application/json\r\nVersion: 0.163.0\r\n" +
			imageTurn + "Authorization: Bearer synthetic\r\nChatgpt-Account-Id: synthetic\r\nOriginator: codex-tui\r\nAccept: */*\r\nCookie: synthetic=value\r\nX-Operator: One\r\nX-Operator: Two\r\n\r\n{}"
		raw.data.Reset()
		if _, err := conn.Write([]byte(input)); err != nil {
			t.Fatal(err)
		}
		wire := raw.data.String()
		var names []string
		for _, line := range strings.Split(strings.SplitN(wire, "\r\n\r\n", 2)[0], "\r\n")[1:] {
			names = append(names, strings.SplitN(line, ":", 2)[0])
		}
		if strings.Contains(path, "/images/") {
			want := []string{"version", "x-codex-image-turn-id", "authorization", "chatgpt-account-id", "content-type", "originator", "user-agent", "accept", "cookie", "x-operator", "x-operator", "host", "content-length"}
			if !reflect.DeepEqual(names, want) {
				t.Fatalf("image header order: %v", names)
			}
		} else {
			want := []string{"version", "accept", "content-type", "authorization", "chatgpt-account-id", "originator", "user-agent", "cookie", "x-operator", "x-operator", "host", "content-length"}
			if !reflect.DeepEqual(names, want) {
				t.Fatalf("Responses header order: %v", names)
			}
		}
		if !strings.HasSuffix(wire, "\r\n\r\n{}") || !strings.Contains(wire, "x-operator: One\r\nx-operator: Two") || !strings.Contains(wire, "user-agent: admin/1") {
			t.Fatal("payload/overrides changed")
		}
	}
}

func TestCodexAlphaSearchHeaderOrderOnReusedConnection(t *testing.T) {
	raw := &codexHeaderCaptureConn{}
	conn := httpwire.NewOrderedRequestConnWithNames(raw, CodexHTTPHeaderOrder)
	for _, path := range []string{"/backend-api/codex/alpha/search", "/responses", "/backend-api/codex/alpha/search?test=1"} {
		raw.data.Reset()
		input := "POST " + path + " HTTP/1.1\r\nHost: test\r\nUser-Agent: native/1\r\nVersion: 0.163.0\r\nX-Codex-Turn-Metadata: {}\r\nAuthorization: Bearer synthetic\r\nChatgpt-Account-Id: synthetic\r\nContent-Type: application/json\r\nOriginator: codex-tui\r\nAccept: */*\r\nCookie: synthetic=value\r\nX-Operator: one\r\nX-Operator: two\r\nContent-Length: 2\r\n\r\n{}"
		if _, err := conn.Write([]byte(input)); err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, line := range strings.Split(strings.SplitN(raw.data.String(), "\r\n\r\n", 2)[0], "\r\n")[1:] {
			names = append(names, strings.SplitN(line, ":", 2)[0])
		}
		if strings.Contains(path, "/alpha/search") {
			want := []string{"version", "x-codex-turn-metadata", "authorization", "chatgpt-account-id", "content-type", "originator", "user-agent", "accept", "cookie", "x-operator", "x-operator", "host", "content-length"}
			if !reflect.DeepEqual(names, want) {
				t.Fatalf("search header order: %v", names)
			}
		} else {
			want := []string{"version", "x-codex-turn-metadata", "accept", "content-type", "authorization", "chatgpt-account-id", "originator", "user-agent", "cookie", "x-operator", "x-operator", "host", "content-length"}
			if !reflect.DeepEqual(names, want) {
				t.Fatalf("Responses template changed: %v", names)
			}
		}
		if !strings.HasSuffix(raw.data.String(), "\r\n\r\n{}") || !strings.Contains(raw.data.String(), "x-operator: one\r\nx-operator: two") {
			t.Fatal("custom header or body changed")
		}
	}
}

func TestCodexResponsesHeaderOrderMatchesNativeOpenAIProvider(t *testing.T) {
	raw := &codexHeaderCaptureConn{}
	conn := httpwire.NewOrderedRequestConnWithNames(raw, CodexHTTPHeaderOrder)
	input := "POST /backend-api/codex/responses HTTP/1.1\r\n" +
		"Host: chatgpt.com\r\n" +
		"Version: 0.163.0\r\n" +
		"X-Codex-Beta-Features: remote_compaction_v2\r\n" +
		"X-Codex-Window-Id: thread:0\r\n" +
		"X-Codex-Turn-Metadata: {}\r\n" +
		"X-OpenAI-Internal-Codex-Responses-Lite: true\r\n" +
		"X-Codex-Routing-Hint: model=gpt-6-luna\r\n" +
		"X-Client-Request-Id: thread\r\n" +
		"Session-Id: thread\r\n" +
		"Thread-Id: thread\r\n" +
		"Accept: text/event-stream\r\n" +
		"Content-Encoding: zstd\r\n" +
		"Content-Type: application/json\r\n" +
		"Authorization: Bearer synthetic\r\n" +
		"Chatgpt-Account-Id: synthetic\r\n" +
		"Originator: codex-tui\r\n" +
		"User-Agent: codex-tui/0.163.0 native\r\n" +
		"Cookie: synthetic=value\r\n" +
		"Content-Length: 2\r\n" +
		"\r\n{}"
	if _, err := conn.Write([]byte(input)); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, line := range strings.Split(strings.SplitN(raw.data.String(), "\r\n\r\n", 2)[0], "\r\n")[1:] {
		names = append(names, strings.SplitN(line, ":", 2)[0])
	}
	want := []string{
		"version", "x-codex-beta-features", "x-codex-window-id",
		"x-codex-turn-metadata", "x-openai-internal-codex-responses-lite",
		"x-codex-routing-hint", "x-client-request-id", "session-id", "thread-id",
		"accept", "content-encoding", "content-type", "authorization",
		"chatgpt-account-id", "originator", "user-agent", "cookie", "host",
		"content-length",
	}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("native Responses header order: %v", names)
	}
}

func TestCodexWebsocketHeaderOrderMatchesNativeOpenAIProvider(t *testing.T) {
	input := "GET /responses HTTP/1.1\r\n" +
		"Host: chatgpt.com\r\n" +
		"Connection: Upgrade\r\n" +
		"Upgrade: websocket\r\n" +
		"Sec-WebSocket-Version: 13\r\n" +
		"Sec-WebSocket-Key: synthetic\r\n" +
		"Cookie: synthetic=value\r\n" +
		"Chatgpt-Account-Id: synthetic\r\n" +
		"Authorization: Bearer synthetic\r\n" +
		"User-Agent: codex-tui/0.163.0 native\r\n" +
		"Originator: codex-tui\r\n" +
		"Version: 0.163.0\r\n" +
		"X-Codex-Beta-Features: remote_compaction_v2\r\n" +
		"X-Client-Request-Id: thread\r\n" +
		"Session-Id: thread\r\n" +
		"Thread-Id: thread\r\n" +
		"X-Codex-Window-Id: thread:0\r\n" +
		"X-Codex-Turn-Metadata: {}\r\n" +
		"X-Codex-Routing-Hint: model=gpt-6-luna\r\n" +
		"OpenAI-Beta: responses_websockets=2026-02-06\r\n" +
		"Sec-WebSocket-Extensions: permessage-deflate\r\n" +
		"\r\n"
	wire := httpwire.ReorderRequestHeader([]byte(input), codexWebsocketHeaderOrder)
	var names []string
	for _, line := range strings.Split(strings.SplitN(string(wire), "\r\n\r\n", 2)[0], "\r\n")[1:] {
		names = append(names, strings.SplitN(line, ":", 2)[0])
	}
	want := []string{
		"Host", "Connection", "Upgrade", "Sec-WebSocket-Version", "Sec-WebSocket-Key",
		"cookie", "chatgpt-account-id", "authorization", "user-agent", "originator",
		"version", "x-codex-beta-features", "x-client-request-id", "session-id",
		"thread-id", "x-codex-window-id", "x-codex-turn-metadata",
		"x-codex-routing-hint", "openai-beta", "sec-websocket-extensions",
	}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("native WebSocket header order: %v", names)
	}
}
