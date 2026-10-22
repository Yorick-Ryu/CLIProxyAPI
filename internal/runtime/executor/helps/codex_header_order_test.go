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
		input := "POST " + path + " HTTP/1.1\r\nHost: test\r\nUser-Agent: admin/1\r\nContent-Length: 2\r\nContent-Type: application/json\r\nVersion: 0.163.0\r\nX-Codex-Image-Turn-Id: test-turn\r\nAuthorization: Bearer synthetic\r\nChatgpt-Account-Id: synthetic\r\nOriginator: codex-tui\r\nAccept: */*\r\nCookie: synthetic=value\r\nX-Operator: One\r\nX-Operator: Two\r\n\r\n{}"
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
		} else if names[0] != "accept" {
			t.Fatalf("Responses ordering changed: %v", names)
		}
		if !strings.HasSuffix(wire, "\r\n\r\n{}") || !strings.Contains(wire, "x-operator: One\r\nx-operator: Two") || !strings.Contains(wire, "user-agent: admin/1") {
			t.Fatal("payload/overrides changed")
		}
	}
}
