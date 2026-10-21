package helps

import (
	"context"
	stdtls "crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
	tls "github.com/refraction-networking/utls"
)

type codexSessionCacheProbe struct{ calls atomic.Int32 }

func (c *codexSessionCacheProbe) Get(string) (*tls.ClientSessionState, bool) {
	c.calls.Add(1)
	return nil, false
}
func (c *codexSessionCacheProbe) Put(string, *tls.ClientSessionState) { c.calls.Add(1) }

func TestCodexTLSFreshSuccessfulReconnect(t *testing.T) {
	for _, version := range []uint16{stdtls.VersionTLS12, stdtls.VersionTLS13} {
		for _, mode := range []string{"http", "ws"} {
			t.Run(stdtls.VersionName(version)+"/"+mode, func(t *testing.T) {
				var requests atomic.Int32
				upgrader := websocket.Upgrader{}
				server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					if r.TLS.DidResume {
						t.Error("new connection resumed a TLS session")
					}
					if mode == "http" {
						w.Header().Set("Connection", "close")
						_, _ = io.WriteString(w, "ok")
						return
					}
					c, err := upgrader.Upgrade(w, r, nil)
					if err != nil {
						t.Error(err)
						return
					}
					defer c.Close()
					kind, data, err := c.ReadMessage()
					if err != nil {
						t.Error(err)
						return
					}
					if err := c.WriteMessage(kind, data); err != nil {
						t.Error(err)
					}
				}))
				server.TLS = &stdtls.Config{MinVersion: version, MaxVersion: version}
				server.StartTLS()
				defer server.Close()
				roots := x509.NewCertPool()
				roots.AddCert(server.Certificate())
				u, _ := url.Parse(server.URL)
				cache := &codexSessionCacheProbe{}
				cfg := &tls.Config{ServerName: u.Hostname(), RootCAs: roots, ClientSessionCache: cache}
				handshake := func(ctx context.Context, raw net.Conn) (net.Conn, error) {
					if mode == "ws" {
						return codexWebsocketTLSHandshake(ctx, raw, cfg)
					}
					c, err := newCodexLinuxTLSClient(raw, cfg)
					if err == nil {
						err = c.HandshakeContext(ctx)
					}
					return c, err
				}
				transport := &http.Transport{DialTLSContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
					raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
					if err != nil {
						return nil, err
					}
					c, err := handshake(ctx, raw)
					if err != nil {
						raw.Close()
					}
					return c, err
				}}
				defer transport.CloseIdleConnections()
				dialer := websocket.Dialer{
					TLSHandshakeContext: func(ctx context.Context, raw net.Conn, _ string) (net.Conn, error) { return handshake(ctx, raw) },
					WriteHandshake:      WriteCodexWebsocketHandshake,
				}
				for range 3 {
					if mode == "http" {
						req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, nil)
						res, err := transport.RoundTrip(req)
						if err != nil {
							t.Fatal(err)
						}
						body, err := io.ReadAll(res.Body)
						res.Body.Close()
						if err != nil || string(body) != "ok" {
							t.Fatalf("reply %q: %v", body, err)
						}
					} else {
						c, _, err := dialer.DialContext(t.Context(), "wss"+strings.TrimPrefix(server.URL, "https"), nil)
						if err != nil {
							t.Fatal(err)
						}
						if err := c.WriteMessage(websocket.TextMessage, []byte("ok")); err != nil {
							c.Close()
							t.Fatal(err)
						}
						_, body, err := c.ReadMessage()
						c.Close()
						if err != nil || string(body) != "ok" {
							t.Fatalf("echo %q: %v", body, err)
						}
					}
				}
				if requests.Load() != 3 {
					t.Fatalf("requests: %d", requests.Load())
				}
				if cache.calls.Load() != 0 {
					t.Fatal("shared session cache was accessed")
				}
				if cfg.ClientSessionCache != cache {
					t.Fatal("caller config mutated")
				}
			})
		}
	}
}
