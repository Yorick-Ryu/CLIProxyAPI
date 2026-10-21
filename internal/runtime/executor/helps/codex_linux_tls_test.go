package helps

import (
	"context"
	"crypto/x509"
	"encoding/binary"
	"errors"
	utls "github.com/refraction-networking/utls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"
	"time"
)

func TestCodexLinuxHTTPWireHello(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	captured := make(chan []byte, 1)
	go func() {
		defer server.Close()
		header := make([]byte, 5)
		if _, err := io.ReadFull(server, header); err != nil {
			captured <- nil
			return
		}
		body := make([]byte, binary.BigEndian.Uint16(header[3:]))
		_, _ = io.ReadFull(server, body)
		captured <- body
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c := utls.UClient(client, &utls.Config{ServerName: "chatgpt.com"}, utls.HelloCustom)
	if err := c.ApplyPreset(codexLinuxHTTPHello()); err != nil {
		t.Fatal(err)
	}
	_ = c.HandshakeContext(ctx)
	body := <-captured
	if len(body) < 39 {
		t.Fatalf("missing ClientHello: %d bytes", len(body))
	}
	b := body[4+2+32:]
	b = b[1+int(b[0]):]
	take := func() []byte { n := int(binary.BigEndian.Uint16(b)); v := b[2 : 2+n]; b = b[2+n:]; return v }
	u16s := func(b []byte) []uint16 {
		var out []uint16
		for len(b) > 0 {
			out = append(out, binary.BigEndian.Uint16(b))
			b = b[2:]
		}
		return out
	}
	if got := u16s(take()); !reflect.DeepEqual(got, []uint16{4866, 4867, 4865, 49196, 49200, 159, 52393, 52392, 52394, 49195, 49199, 158, 49188, 49192, 107, 49187, 49191, 103, 49162, 49172, 57, 49161, 49171, 51, 157, 156, 61, 60, 53, 47}) {
		t.Fatalf("ciphers %v", got)
	}
	b = b[1+int(b[0]):]
	b = take()
	ext := map[uint16][]byte{}
	var ids []uint16
	for len(b) > 0 {
		id := binary.BigEndian.Uint16(b)
		b = b[2:]
		ext[id] = take()
		ids = append(ids, id)
	}
	if !reflect.DeepEqual(ids, []uint16{65281, 0, 11, 10, 35, 22, 23, 13, 43, 45, 51}) {
		t.Fatalf("extensions %v", ids)
	}
	if got := u16s(ext[10][2:]); !reflect.DeepEqual(got, []uint16{4588, 29, 23, 30, 24, 25, 256, 257}) {
		t.Fatalf("groups %v", got)
	}
	if got := u16s(ext[13][2:]); !reflect.DeepEqual(got, []uint16{2309, 2310, 2308, 1027, 1283, 1539, 2055, 2056, 2074, 2075, 2076, 2057, 2058, 2059, 2052, 2053, 2054, 1025, 1281, 1537, 771, 769, 770, 1026, 1282, 1538}) {
		t.Fatalf("signatures %v", got)
	}
	if got := u16s(ext[43][1:]); !reflect.DeepEqual(got, []uint16{772, 771}) {
		t.Fatalf("versions %v", got)
	}
	b = ext[51][2:]
	var shares [][2]uint16
	for len(b) > 0 {
		group := binary.BigEndian.Uint16(b)
		b = b[2:]
		key := take()
		shares = append(shares, [2]uint16{group, uint16(len(key))})
	}
	if !reflect.DeepEqual(shares, [][2]uint16{{4588, 1216}, {29, 32}}) {
		t.Fatalf("shares %v", shares)
	}
}

func TestCodexLinuxHTTPRejectsUntrustedCertificate(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("unverified connection sent HTTP") }))
	defer server.Close()
	u, _ := url.Parse(server.URL)
	tr := newUtlsRoundTripper("direct")
	tr.codexLinux = true
	conn, err := tr.createConnection(t.Context(), u.Hostname(), u.Host)
	if err == nil {
		_ = conn.Close()
		t.Fatal("untrusted certificate accepted")
	}
	var unknown x509.UnknownAuthorityError
	if !errors.As(err, &unknown) {
		t.Fatalf("expected certificate verification error, got %v", err)
	}
}
