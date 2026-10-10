package websocket

import (
	"bytes"
	"compress/flate"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type compressionBuffer struct{ bytes.Buffer }

func (*compressionBuffer) Close() error { return nil }

func TestContextCompressionRetainsHistory(t *testing.T) {
	payload := make([]byte, 12000)
	_, _ = rand.New(rand.NewSource(42)).Read(payload)
	encode := contextCompressionWriter(15, true)
	decode := contextDecompressionReader(true)
	var firstSize int
	for turn := 0; turn < 6; turn++ {
		var wire compressionBuffer
		writer := encode(&wire, 6)
		if _, err := writer.Write(payload); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		if turn == 0 {
			firstSize = wire.Len()
		} else {
			if wire.Len() >= firstSize/2 {
				t.Fatalf("turn %d failed to reuse history: %d vs %d bytes", turn, wire.Len(), firstSize)
			}
			fresh := decompressNoContextTakeover(bytes.NewReader(wire.Bytes()))
			if got, err := io.ReadAll(fresh); err == nil && bytes.Equal(got, payload) {
				t.Fatal("later message unexpectedly decodes without prior history")
			}
			_ = fresh.Close()
		}
		reader := decode(bytes.NewReader(wire.Bytes()))
		if turn == 0 {
			// Closing a partially consumed message must still update the dictionary.
			if _, err := io.ReadFull(reader, make([]byte, 7)); err != nil {
				t.Fatal(err)
			}
		} else if got, err := io.ReadAll(reader); err != nil || !bytes.Equal(got, payload) {
			t.Fatalf("turn %d decode mismatch: %v", turn, err)
		}
		if err := reader.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestContextCompressionWindowAndResetModes(t *testing.T) {
	for bits := 8; bits <= 15; bits++ {
		for _, retain := range []bool{false, true} {
			t.Run(fmt.Sprintf("bits%d/retain%v", bits, retain), func(t *testing.T) {
				encode := contextCompressionWriter(bits, retain)
				var history []byte
				for turn := 0; turn < 5; turn++ {
					payload := bytes.Repeat([]byte("window-limited-compression-data-"), 3000)
					var wire compressionBuffer
					writer := encode(&wire, 6)
					if _, err := writer.Write(payload); err != nil {
						t.Fatal(err)
					}
					if err := writer.Close(); err != nil {
						t.Fatal(err)
					}
					reader := flate.NewReaderDict(io.MultiReader(&wire, strings.NewReader("\x00\x00\xff\xff\x01\x00\x00\xff\xff")), history)
					got, err := io.ReadAll(reader)
					_ = reader.Close()
					if err != nil || !bytes.Equal(got, payload) {
						t.Fatalf("turn %d: %v", turn, err)
					}
					if retain {
						history = payload[len(payload)-(1<<uint(bits)):]
					}
				}
			})
		}
	}
}

func TestContextNegotiation(t *testing.T) {
	for _, extension := range []string{
		"", "permessage-deflate", "permessage-deflate; client_no_context_takeover",
		"permessage-deflate; server_no_context_takeover",
		"permessage-deflate; server_no_context_takeover; client_no_context_takeover",
		`permessage-deflate; client_max_window_bits="8"; server_max_window_bits=9`,
	} {
		c := &Conn{}
		if err := c.negotiateContextTakeover(http.Header{"Sec-Websocket-Extensions": {extension}}); err != nil {
			t.Fatalf("valid negotiation %q: %v", extension, err)
		}
		if (c.newCompressionWriter != nil) != (extension != "") {
			t.Fatalf("compression mode for %q", extension)
		}
	}
	for _, extension := range []string{
		"unknown", "permessage-deflate, permessage-deflate", "permessage-deflate; unknown",
		"permessage-deflate; client_no_context_takeover=true", "permessage-deflate; server_no_context_takeover=",
		"permessage-deflate; client_max_window_bits", "permessage-deflate; client_max_window_bits=7",
		"permessage-deflate; client_max_window_bits=16", "permessage-deflate; client_max_window_bits=08",
		"permessage-deflate; client_max_window_bits=8; client_max_window_bits=9",
		"permessage-deflate;", "permessage-deflate; server_max_window_bits=invalid",
	} {
		if err := (&Conn{}).negotiateContextTakeover(http.Header{"Sec-Websocket-Extensions": {extension}}); err == nil {
			t.Fatalf("accepted invalid negotiation %q", extension)
		}
	}
}

func TestContextConnectionPreparedPartialAndControl(t *testing.T) {
	var wire bytes.Buffer
	writer := newTestConn(nil, &wire, true)
	reader := newTestConn(&wire, io.Discard, false)
	for _, conn := range []*Conn{writer, reader} {
		if err := conn.negotiateContextTakeover(http.Header{"Sec-Websocket-Extensions": {"permessage-deflate"}}); err != nil {
			t.Fatal(err)
		}
	}
	reader.SetPingHandler(func(string) error { return nil })
	payload := bytes.Repeat([]byte("multi-frame context data with a recurring pattern"), 1800)
	for turn := 0; turn < 6; turn++ {
		if turn == 2 {
			writer.EnableWriteCompression(false)
		} else {
			writer.EnableWriteCompression(true)
		}
		if turn == 4 {
			if err := writer.SetCompressionLevel(6); err != nil {
				t.Fatal(err)
			}
		}
		pm, err := NewPreparedMessage(TextMessage, payload)
		if err != nil {
			t.Fatal(err)
		}
		if err := writer.WritePreparedMessage(pm); err != nil {
			t.Fatal(err)
		}
		if err := writer.WriteMessage(PingMessage, []byte("ping")); err != nil {
			t.Fatal(err)
		}
	}
	for turn := 0; turn < 6; turn++ {
		_, message, err := reader.NextReader()
		if err != nil {
			t.Fatal(err)
		}
		if turn == 0 {
			_, err = io.ReadFull(message, make([]byte, 3))
		} else {
			var got []byte
			got, err = io.ReadAll(message)
			if !bytes.Equal(got, payload) {
				t.Fatalf("turn %d mismatch", turn)
			}
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestContextDialOfferAndRoundTrips(t *testing.T) {
	for _, extension := range []string{"", "permessage-deflate", "permessage-deflate; client_no_context_takeover; server_no_context_takeover"} {
		t.Run(extension, func(t *testing.T) {
			done := make(chan error, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("Sec-WebSocket-Extensions"); got != "permessage-deflate; client_max_window_bits" {
					done <- fmt.Errorf("unexpected offer: %s", got)
					return
				}
				nc, rw, err := w.(http.Hijacker).Hijack()
				if err != nil {
					done <- err
					return
				}
				defer nc.Close()
				_, _ = fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n", computeAcceptKey(r.Header.Get("Sec-WebSocket-Key")))
				if extension != "" {
					_, _ = fmt.Fprintf(rw, "Sec-WebSocket-Extensions: %s\r\n", extension)
				}
				_, _ = fmt.Fprint(rw, "\r\n")
				_ = rw.Flush()
				conn := newConn(nc, true, 1024, 1024, nil, rw.Reader, nil)
				if extension != "" {
					retain := !strings.Contains(extension, "no_context_takeover")
					conn.newCompressionWriter = contextCompressionWriter(15, retain)
					conn.newDecompressionReader = contextDecompressionReader(retain)
				}
				for i := 0; i < 6; i++ {
					kind, data, err := conn.ReadMessage()
					if err == nil {
						err = conn.WriteMessage(kind, data)
					}
					if err != nil {
						done <- err
						return
					}
				}
				done <- nil
			}))
			defer server.Close()
			dialer := Dialer{EnableCompression: true, EnableContextTakeover: true}
			conn, _, err := dialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			for i := 0; i < 6; i++ {
				payload := bytes.Repeat([]byte(fmt.Sprintf("turn-%d recurring data", i)), 2000)
				if err := conn.WriteMessage(TextMessage, payload); err != nil {
					t.Fatal(err)
				}
				if _, got, err := conn.ReadMessage(); err != nil || !bytes.Equal(got, payload) {
					t.Fatalf("turn %d: %v", i, err)
				}
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}
