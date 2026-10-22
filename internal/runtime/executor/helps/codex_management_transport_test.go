package helps

import (
	"bufio"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"reflect"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/httpwire"
)

func TestCodexManagementQueryScope(t *testing.T) {
	for _, tc := range []struct {
		method, target string
		want           bool
	}{
		{"GET", "https://chatgpt.com/backend-api/wham/usage", true},
		{"GET", "https://chatgpt.com/backend-api/subscriptions?account_id=synthetic", true},
		{"GET", "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits", true},
		{"POST", "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume", false},
		{"GET", "https://chatgpt.com/backend-api/codex/responses", false},
		{"GET", "https://chatgpt.com.evil.test/backend-api/wham/usage", false},
		{"GET", "http://chatgpt.com/backend-api/wham/usage", false},
		{"GET", "https://chatgpt.com/backend-api/wham/%75sage", false},
	} {
		req, _ := http.NewRequest(tc.method, tc.target, nil)
		if got := CodexManagementQuery(req); got != tc.want {
			t.Errorf("%s %s: %v", tc.method, tc.target, got)
		}
	}
}

func TestCodexManagementProxyWireHello(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	captured := make(chan []byte, 1)
	go func() {
		c, err := listener.Accept()
		if err != nil {
			captured <- nil
			return
		}
		defer c.Close()
		reader := bufio.NewReader(c)
		req, err := http.ReadRequest(reader)
		if err != nil || req.Method != "CONNECT" || req.Host != "chatgpt.com:443" {
			captured <- nil
			return
		}
		if _, err = io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
			captured <- nil
			return
		}
		hdr := make([]byte, 5)
		if _, err = io.ReadFull(reader, hdr); err != nil {
			captured <- nil
			return
		}
		data := make([]byte, binary.BigEndian.Uint16(hdr[3:]))
		if _, err = io.ReadFull(reader, data); err != nil {
			captured <- nil
			return
		}
		captured <- data
	}()
	tr := NewCodexManagementTransport("http://" + listener.Addr().String())
	req, _ := http.NewRequestWithContext(t.Context(), "GET", "https://chatgpt.com/backend-api/wham/usage", nil)
	if _, err := tr.RoundTrip(req); err == nil {
		t.Fatal("closed handshake should fail")
	}
	data := <-captured
	if len(data) < 40 {
		t.Fatal("missing ClientHello through proxy")
	}
	b := data[4+2+32:]
	b = b[1+int(b[0]):]
	n := int(binary.BigEndian.Uint16(b))
	b = b[2 : 2+n]
	var ciphers []uint16
	for len(b) > 0 {
		ciphers = append(ciphers, binary.BigEndian.Uint16(b))
		b = b[2:]
	}
	if !reflect.DeepEqual(ciphers, codexLinuxHTTPHello().CipherSuites) {
		t.Fatalf("ciphers %v", ciphers)
	}
}

func TestCodexManagementHeadersPreserved(t *testing.T) {
	input := []byte("GET /backend-api/wham/usage HTTP/1.1\r\nHost: chatgpt.com\r\nUser-Agent: operator\r\nX-Custom: one\r\nX-Custom: two\r\nAuthorization: Bearer synthetic\r\n\r\n")
	got := string(httpwire.ReorderRequestHeader(input, codexManagementHeaderOrder))
	want := "GET /backend-api/wham/usage HTTP/1.1\r\nuser-agent: operator\r\nauthorization: Bearer synthetic\r\nx-custom: one\r\nx-custom: two\r\nhost: chatgpt.com\r\n\r\n"
	if got != want {
		t.Fatalf("wire mismatch: %q", got)
	}
}
