package executor

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

const codexWebsocketMaxPayloadBytes = 20_000_000

func TestCodexWebsocketPayloadSizeConfiguration(t *testing.T) {
	payload := make([]byte, 20_000_001)
	for _, tc := range []struct {
		name      string
		cfg       *config.Config
		wantError bool
	}{
		{"nil", nil, false},
		{"omitted", &config.Config{}, false},
		{"negative", &config.Config{Codex: config.CodexConfig{WebsocketMaxMessageBytes: -1}}, false},
		{"20MB", &config.Config{Codex: config.CodexConfig{WebsocketMaxMessageBytes: 20_000_000}}, true},
		{"exact", &config.Config{Codex: config.CodexConfig{WebsocketMaxMessageBytes: 20_000_001}}, false},
		{"30MB", &config.Config{Codex: config.CodexConfig{WebsocketMaxMessageBytes: 30_000_000}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateCodexWebsocketPayloadSize(tc.cfg, payload); (err != nil) != tc.wantError {
				t.Fatalf("unexpected size check: %v", err)
			}
		})
	}
}

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
				executor := NewCodexWebsocketsExecutor(&config.Config{Codex: config.CodexConfig{WebsocketMaxMessageBytes: codexWebsocketMaxPayloadBytes}})
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
				// A compressible oversized message must be rejected before any frame
				// is sent, without preventing the next valid message on this socket.
				oversized := bytes.Repeat([]byte("x"), codexWebsocketMaxPayloadBytes+1)
				err = writeCodexWebsocketMessage(executor.cfg, session, conn, closer, oversized)
				var status interface{ StatusCode() int }
				if !errors.As(err, &status) || status.StatusCode() != http.StatusRequestEntityTooLarge {
					t.Fatalf("oversized message status: %v", err)
				}
				var scoped cliproxyexecutor.RequestScopedError
				if !errors.As(err, &scoped) || !scoped.IsRequestScoped() || shouldRetryCodexWebsocketSend(err) {
					t.Fatalf("oversized message must be request-scoped and non-retryable: %v", err)
				}
				var body struct {
					Error struct {
						Code   string `json:"code"`
						Actual int    `json:"actual_bytes"`
						Limit  int    `json:"limit_bytes"`
					} `json:"error"`
				}
				if json.Unmarshal([]byte(err.Error()), &body) != nil || body.Error.Code != "message_too_big" || body.Error.Actual != len(oversized) || body.Error.Limit != codexWebsocketMaxPayloadBytes {
					t.Fatalf("unexpected size error: %v", err)
				}
				if session != nil {
					session.setUpstreamDisconnectError(conn, &websocket.CloseError{Code: websocket.CloseMessageTooBig})
					if mapped := mapCodexWebsocketWriteError(session, conn, err); mapped != err {
						t.Fatal("upstream close must not replace local size details")
					}
				}
				for _, payload := range [][]byte{
					[]byte(`{"type":"response.create","model":"test-model","input":"hello"}`),
					bytes.Repeat([]byte("b"), codexWebsocketFragmentSize),
					bytes.Repeat([]byte("c"), codexWebsocketFragmentSize+1),
					[]byte(`{"type":"response.create","model":"test-model","input":"` + strings.Repeat("context ", 128*1024) + `"}`),
					[]byte(`{"type":"response.create","model":"test-model","previous_response_id":"resp-1","input":"continue"}`),
					bytes.Repeat([]byte("x"), codexWebsocketMaxPayloadBytes-1),
					bytes.Repeat([]byte("x"), codexWebsocketMaxPayloadBytes),
				} {
					recorded.received.Reset()
					written := make(chan error, 1)
					go func() { written <- writeCodexWebsocketMessage(executor.cfg, session, conn, closer, payload) }()
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
					assertCodexWebsocketFrames(t, wire, len(payload), negotiated)
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
				// Removing the setting must also disable the send-path guard on
				// an existing connection, even for messages above the old limit.
				written := make(chan error, 1)
				go func() { written <- writeCodexWebsocketMessage(&config.Config{}, session, conn, closer, oversized) }()
				_, received, err := peer.ReadMessage()
				if err != nil {
					t.Fatal(err)
				}
				if err := <-written; err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(received, oversized) {
					t.Fatal("disabled limit changed the message")
				}
			})
		}
	}
}

// assertCodexWebsocketFrames checks wire headers independently of Gorilla's
// reassembly. Intermediate DEFLATE fragments retain the sync-flush trailer.
func assertCodexWebsocketFrames(t *testing.T, wire []byte, payloadSize int, compressed bool) {
	t.Helper()
	fragmentCount := max(1, (payloadSize+codexWebsocketFragmentSize-1)/codexWebsocketFragmentSize)
	for index := range fragmentCount {
		if len(wire) < 6 {
			t.Fatalf("missing wire frame %d of %d", index+1, fragmentCount)
		}
		var wantHeader byte
		if index == 0 {
			wantHeader = websocket.TextMessage
			if compressed {
				wantHeader |= 0x40
			}
		}
		final := index == fragmentCount-1
		if final {
			wantHeader |= 0x80
		}
		if wire[0] != wantHeader || wire[1]&0x80 == 0 {
			t.Fatalf("frame %d header = %02x %02x, want %02x with client mask", index+1, wire[0], wire[1], wantHeader)
		}
		size := uint64(wire[1] & 0x7f)
		offset := 2
		switch size {
		case 126:
			size = uint64(binary.BigEndian.Uint16(wire[2:4]))
			offset += 2
		case 127:
			if len(wire) < 14 {
				t.Fatal("short frame header")
			}
			size = binary.BigEndian.Uint64(wire[2:10])
			offset += 8
		}
		if len(wire) < offset+4 || size > uint64(len(wire)-offset-4) {
			t.Fatalf("truncated frame %d", index+1)
		}
		mask := wire[offset : offset+4]
		data := wire[offset+4 : offset+4+int(size)]
		wire = wire[offset+4+int(size):]
		if !compressed {
			want := min(codexWebsocketFragmentSize, payloadSize-index*codexWebsocketFragmentSize)
			if len(data) != want {
				t.Fatalf("frame %d size = %d, want %d", index+1, len(data), want)
			}
		} else if !final {
			if len(data) < 4 {
				t.Fatal("missing DEFLATE sync-flush trailer")
			}
			for i, expected := range []byte{0, 0, 0xff, 0xff} {
				pos := len(data) - 4 + i
				if data[pos]^mask[pos%4] != expected {
					t.Fatalf("frame %d did not retain the DEFLATE sync-flush trailer", index+1)
				}
			}
		}
	}
	if len(wire) != 0 {
		t.Fatalf("unexpected bytes after final frame: %d", len(wire))
	}
}

func TestCodexWebsocketUpstreamMessageLimit(t *testing.T) {
	for _, tc := range []struct {
		name        string
		header      string
		local       int64
		first       string
		second      string
		limitSource string
	}{
		{"upstream only", "8", 0, "123456789", "12345678", "upstream"},
		{"zero", "0", 20, "1", "", "upstream"},
		{"malformed header", "invalid", 0, "", "123456789", ""},
		{"tighter local", "8", 6, "1234567", "123456", "local"},
		{"tighter upstream", "6", 8, "1234567", "123456", "upstream"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			peers := make(chan *websocket.Conn, 1)
			upgrader := websocket.Upgrader{EnableCompression: true}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, errUpgrade := upgrader.Upgrade(w, r, http.Header{codexWebsocketMaxMessageBytesHeader: {tc.header}})
				if errUpgrade != nil {
					t.Error(errUpgrade)
					return
				}
				peers <- conn
			}))
			defer server.Close()
			executor := NewCodexWebsocketsExecutor(&config.Config{Codex: config.CodexConfig{WebsocketMaxMessageBytes: tc.local}})
			conn, closer, _, errDial := executor.dialCodexWebsocket(context.Background(), nil, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
			if errDial != nil {
				t.Fatal(errDial)
			}
			defer closer.Close()
			peer := <-peers
			defer peer.Close()
			sess := &codexWebsocketSession{}
			if tc.first != "" {
				errSize := writeCodexWebsocketMessage(executor.cfg, sess, conn, closer, []byte(tc.first))
				var scoped codexWebsocketMessageTooBigError
				if !errors.As(errSize, &scoped) || scoped.StatusCode() != http.StatusRequestEntityTooLarge || !strings.Contains(scoped.Error(), tc.limitSource+" uncompressed size limit") {
					t.Fatalf("upstream limit error: %v", errSize)
				}
			}
			written := make(chan error, 1)
			go func() {
				written <- writeCodexWebsocketMessage(executor.cfg, sess, conn, closer, []byte(tc.second))
			}()
			_, payload, errRead := peer.ReadMessage()
			if errRead != nil {
				t.Fatal(errRead)
			}
			if errWrite := <-written; errWrite != nil {
				t.Fatal(errWrite)
			}
			if string(payload) != tc.second {
				t.Fatalf("wire message = %q, want %q", payload, tc.second)
			}
		})
	}
}
