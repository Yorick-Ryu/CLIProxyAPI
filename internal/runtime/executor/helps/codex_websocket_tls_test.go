package helps

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/proxy"
)

func TestCodexWebsocketTLSWireHello(t *testing.T) {
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
	_, _ = CodexWebsocketTLSHandshake(ctx, client, "chatgpt.com")
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
	if got := u16s(take()); !reflect.DeepEqual(got, []uint16{4866, 4865, 4867, 49196, 49195, 52393, 49200, 49199, 52392, 255}) {
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
	slices.Sort(ids)
	if !reflect.DeepEqual(ids, []uint16{0, 5, 10, 11, 13, 23, 35, 43, 45, 51}) {
		t.Fatalf("extensions %v", ids)
	}
	if got := u16s(ext[10][2:]); !reflect.DeepEqual(got, []uint16{4588, 29, 23, 24}) {
		t.Fatalf("groups %v", got)
	}
	if got := u16s(ext[13][2:]); !reflect.DeepEqual(got, []uint16{1283, 1027, 1539, 2055, 2054, 2053, 2052, 1537, 1281, 1025, 2308, 2309, 2310}) {
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

func TestCodexWebsocketTLSRoundTrip(t *testing.T) {
	for _, version := range []uint16{tls.VersionTLS12, tls.VersionTLS13} {
		for _, route := range []string{"direct", "http", "socks5"} {
			t.Run(fmtName(version, route), func(t *testing.T) {
				upgrader := websocket.Upgrader{EnableCompression: true}
				upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					c, err := upgrader.Upgrade(w, r, nil)
					if err != nil {
						return
					}
					defer c.Close()
					for {
						kind, data, err := c.ReadMessage()
						if err != nil {
							return
						}
						if err = c.WriteMessage(kind, data); err != nil {
							return
						}
					}
				}))
				upstream.TLS = &tls.Config{MinVersion: version, MaxVersion: version}
				upstream.StartTLS()
				defer upstream.Close()
				roots := x509.NewCertPool()
				roots.AddCert(upstream.Certificate())
				d := websocket.Dialer{EnableCompression: true, EnableContextTakeover: true, HandshakeTimeout: 5 * time.Second}
				d.TLSHandshakeContext = func(ctx context.Context, c net.Conn, host string) (net.Conn, error) {
					return codexWebsocketTLSHandshake(ctx, c, &utls.Config{ServerName: host, RootCAs: roots})
				}
				if route == "http" {
					proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.Method != "CONNECT" {
							t.Error("TLS sent before CONNECT")
							w.WriteHeader(400)
							return
						}
						dst, err := net.Dial("tcp", r.Host)
						if err != nil {
							w.WriteHeader(502)
							return
						}
						defer dst.Close()
						raw, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							return
						}
						defer raw.Close()
						_, _ = io.WriteString(raw, "HTTP/1.1 200 Connection Established\r\n\r\n")
						done := make(chan struct{})
						go func() { _, _ = io.Copy(dst, raw); _ = dst.Close(); close(done) }()
						_, _ = io.Copy(raw, dst)
						_ = raw.Close()
						<-done
					}))
					defer proxyServer.Close()
					u, _ := url.Parse(proxyServer.URL)
					d.Proxy = http.ProxyURL(u)
				}
				if route == "socks5" {
					listener, err := net.Listen("tcp", "127.0.0.1:0")
					if err != nil {
						t.Fatal(err)
					}
					defer listener.Close()
					go func() {
						for {
							raw, err := listener.Accept()
							if err != nil {
								return
							}
							go func() {
								defer raw.Close()
								h := make([]byte, 2)
								if _, err := io.ReadFull(raw, h); err != nil {
									return
								}
								methods := make([]byte, int(h[1]))
								if _, err := io.ReadFull(raw, methods); err != nil {
									return
								}
								_, _ = raw.Write([]byte{5, 0})
								req := make([]byte, 4)
								if _, err := io.ReadFull(raw, req); err != nil {
									return
								}
								var size int
								switch req[3] {
								case 1:
									size = 4
								case 4:
									size = 16
								case 3:
									b := make([]byte, 1)
									if _, err := io.ReadFull(raw, b); err != nil {
										return
									}
									size = int(b[0])
								default:
									return
								}
								address := make([]byte, size+2)
								if _, err := io.ReadFull(raw, address); err != nil {
									return
								}
								// The fixture forwards only to its own TLS server.
								dst, err := net.Dial("tcp", strings.TrimPrefix(upstream.URL, "https://"))
								if err != nil {
									return
								}
								defer dst.Close()
								_, _ = raw.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0})
								done := make(chan struct{})
								go func() { _, _ = io.Copy(dst, raw); _ = dst.Close(); close(done) }()
								_, _ = io.Copy(raw, dst)
								_ = raw.Close()
								<-done
							}()
						}
					}()
					sd, err := proxy.SOCKS5("tcp", listener.Addr().String(), nil, proxy.Direct)
					if err != nil {
						t.Fatal(err)
					}
					d.NetDialContext = sd.(proxy.ContextDialer).DialContext
				}
				target := "wss" + strings.TrimPrefix(upstream.URL, "https")
				c, _, err := d.DialContext(context.Background(), target, nil)
				if err != nil {
					t.Fatal(err)
				}
				for i := 0; i < 2; i++ {
					if err = c.WriteMessage(websocket.TextMessage, []byte("repeat compressed websocket content")); err != nil {
						t.Fatal(err)
					}
					_, got, err := c.ReadMessage()
					if err != nil || string(got) != "repeat compressed websocket content" {
						t.Fatalf("echo %q: %v", got, err)
					}
				}
				_ = c.Close()
				d.TLSHandshakeContext = CodexWebsocketTLSHandshake
				if c, _, err = d.DialContext(context.Background(), target, nil); err == nil {
					_ = c.Close()
					t.Fatal("untrusted certificate accepted")
				}
				d.TLSHandshakeContext = func(ctx context.Context, c net.Conn, _ string) (net.Conn, error) {
					return codexWebsocketTLSHandshake(ctx, c, &utls.Config{ServerName: "wrong.example", RootCAs: roots})
				}
				if c, _, err = d.DialContext(context.Background(), target, nil); err == nil {
					_ = c.Close()
					t.Fatal("wrong hostname accepted")
				}
			})
		}
	}
}
func fmtName(version uint16, route string) string {
	if version == tls.VersionTLS12 {
		return "TLS12_" + route
	}
	return "TLS13_" + route
}

func TestCodexWebsocketTLSCancellation(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := CodexWebsocketTLSHandshake(ctx, client, "chatgpt.com"); err == nil {
		t.Fatal("cancelled handshake succeeded")
	}
}
