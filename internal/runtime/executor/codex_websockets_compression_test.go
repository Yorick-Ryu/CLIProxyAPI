package executor

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

type codexCompressionListener struct{ net.Listener }

func (l codexCompressionListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &codexCompressionConn{Conn: conn}, nil
}

type codexCompressionConn struct {
	net.Conn
	received bytes.Buffer
}

func (c *codexCompressionConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.received.Write(p[:n])
	return n, err
}

func TestCodexWebsocketRequestCompression(t *testing.T) {
	for _, negotiated := range []bool{true, false} {
		for _, persistent := range []bool{true, false} {
			t.Run(fmt.Sprintf("negotiated=%t/session=%t", negotiated, persistent), func(t *testing.T) {
				peers := make(chan *websocket.Conn, 1)
				upgrader := websocket.Upgrader{EnableCompression: negotiated}
				server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					conn, err := upgrader.Upgrade(w, r, nil)
					if err != nil {
						t.Errorf("upgrade: %v", err)
						return
					}
					peers <- conn
				}))
				server.Listener = codexCompressionListener{server.Listener}
				server.Start()
				defer server.Close()
				executor := NewCodexWebsocketsExecutor(&config.Config{})
				conn, closer, response, err := executor.dialCodexWebsocket(context.Background(), nil, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
				if err != nil {
					t.Fatalf("dial: %v", err)
				}
				defer closer.Close()
				if got := strings.Contains(response.Header.Get("Sec-WebSocket-Extensions"), "permessage-deflate"); got != negotiated {
					t.Fatalf("negotiated = %t, want %t", got, negotiated)
				}
				peer := <-peers
				defer peer.Close()
				recorded := peer.NetConn().(*codexCompressionConn)
				var session *codexWebsocketSession
				if persistent {
					session = &codexWebsocketSession{}
				}
				for _, payload := range [][]byte{
					[]byte(`{"type":"response.create","model":"test-model","input":"hello"}`),
					[]byte(`{"type":"response.create","model":"test-model","input":"` + strings.Repeat("context ", 128*1024) + `"}`),
					[]byte(`{"type":"response.create","model":"test-model","previous_response_id":"resp-1","input":"continue"}`),
				} {
					recorded.received.Reset()
					written := make(chan error, 1)
					go func() { written <- writeCodexWebsocketMessage(session, conn, payload) }()
					messageType, received, err := peer.ReadMessage()
					if err != nil {
						t.Fatalf("read upstream request: %v", err)
					}
					if err := <-written; err != nil {
						t.Fatalf("write upstream request: %v", err)
					}
					if messageType != websocket.TextMessage || !bytes.Equal(received, payload) {
						t.Fatal("upstream request type or contents changed")
					}
					wire := recorded.received.Bytes()
					if len(wire) < 2 {
						t.Fatal("missing wire frame")
					}
					if got := wire[0]&0x40 != 0; got != negotiated {
						t.Fatalf("request RSV1 = %t, want %t", got, negotiated)
					}
					if wire[1]&0x80 == 0 {
						t.Fatal("upstream request frames must remain masked")
					}
					if negotiated && len(payload) > 1024 && len(wire) >= len(payload)/10 {
						t.Fatalf("repetitive request did not shrink: %d wire bytes, %d input bytes", len(wire), len(payload))
					}

					// Receiving negotiated compressed replies must still work on the same connection.
					go func() { written <- peer.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed"}`)) }()
					_, reply, err := conn.ReadMessage()
					if err != nil {
						t.Fatalf("read upstream reply: %v", err)
					}
					if err := <-written; err != nil {
						t.Fatalf("write upstream reply: %v", err)
					}
					if string(reply) != `{"type":"response.completed"}` {
						t.Fatal("upstream reply changed")
					}
				}
			})
		}
	}
}
