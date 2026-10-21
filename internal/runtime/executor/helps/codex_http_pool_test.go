package helps

import (
	"context"
	stdtls "crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tls "github.com/refraction-networking/utls"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	auth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type poolTestFixture struct {
	server      *httptest.Server
	dial        codexTLSDial
	dials       atomic.Int32
	mu          sync.Mutex
	connections []*poolTestConn
}
type poolTestConn struct {
	net.Conn
	once   sync.Once
	closed chan struct{}
}

func (c *poolTestConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { close(c.closed) })
	return err
}

func newPoolTestFixture(t *testing.T, protocol string, handler http.HandlerFunc) *poolTestFixture {
	t.Helper()
	f := &poolTestFixture{}
	f.server = httptest.NewUnstartedServer(handler)
	f.server.EnableHTTP2 = protocol == "h2"
	f.server.TLS = &stdtls.Config{NextProtos: []string{}}
	if protocol != "" {
		f.server.TLS.NextProtos = []string{protocol}
	}
	f.server.StartTLS()
	t.Cleanup(f.server.Close)
	roots := x509.NewCertPool()
	roots.AddCert(f.server.Certificate())
	u, _ := url.Parse(f.server.URL)
	f.dial = func(ctx context.Context, _, _ string) (net.Conn, string, error) {
		raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", u.Host)
		if err != nil {
			return nil, "", err
		}
		tracked := &poolTestConn{Conn: raw, closed: make(chan struct{})}
		f.mu.Lock()
		f.connections = append(f.connections, tracked)
		f.mu.Unlock()
		c := tls.UClient(tracked, &tls.Config{ServerName: u.Hostname(), RootCAs: roots}, tls.HelloChrome_Auto)
		if err := c.HandshakeContext(ctx); err != nil {
			_ = c.Close()
			return nil, "", err
		}
		f.dials.Add(1)
		return c, c.ConnectionState().NegotiatedProtocol, nil
	}
	return f
}
func poolRouter(t *testing.T, f *poolTestFixture) *codexHTTPPoolRouter {
	t.Helper()
	r := &codexHTTPPoolRouter{account: [32]byte{1}, dial: f.dial, cache: newCodexHTTPPoolCache(8)}
	t.Cleanup(r.CloseIdleConnections)
	return r
}
func poolGET(t *testing.T, r http.RoundTripper, ctx context.Context, target string) *http.Response {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	response, err := r.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	return response
}
func readPoolReply(t *testing.T, r *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
func awaitPoolClosed(t *testing.T, c *poolTestConn) {
	t.Helper()
	select {
	case <-c.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("connection was not closed")
	}
}

func TestCodexHTTPPoolReusesNegotiatedConnection(t *testing.T) {
	for _, protocol := range []string{"h2", "http/1.1", ""} {
		t.Run(protocol, func(t *testing.T) {
			f := newPoolTestFixture(t, protocol, func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Accept-Encoding") != "" {
					t.Error("automatic response compression returned")
				}
				if (r.ProtoMajor == 2) != (protocol == "h2") {
					t.Errorf("wrong protocol %s", r.Proto)
				}
				_, _ = io.WriteString(w, "ok")
			})
			router := poolRouter(t, f)
			for range 3 {
				if readPoolReply(t, poolGET(t, router, t.Context(), "https://chatgpt.com/responses")) != "ok" {
					t.Fatal("wrong body")
				}
			}
			if f.dials.Load() != 1 {
				t.Fatalf("handshakes=%d want 1", f.dials.Load())
			}
			router.CloseIdleConnections()
			awaitPoolClosed(t, f.connections[0])
			readPoolReply(t, poolGET(t, router, t.Context(), "https://chatgpt.com/responses"))
			if f.dials.Load() != 2 {
				t.Fatal("idle close did not allow a fresh connection")
			}
		})
	}
}

func TestCodexHTTPPoolH2CancellationDoesNotCloseOtherStream(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	f := newPoolTestFixture(t, "h2", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hold" {
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			close(started)
			select {
			case <-r.Context().Done():
			case <-release:
			}
			return
		}
		_, _ = io.WriteString(w, "other")
	})
	router := poolRouter(t, f)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	held := poolGET(t, router, ctx, "https://chatgpt.com/hold")
	<-started
	if readPoolReply(t, poolGET(t, router, t.Context(), "https://chatgpt.com/other")) != "other" {
		t.Fatal("concurrent stream failed")
	}
	cancel()
	_, err := io.ReadAll(held.Body)
	_ = held.Body.Close()
	if err == nil {
		t.Fatal("cancelled stream did not stop")
	}
	readPoolReply(t, poolGET(t, router, t.Context(), "https://chatgpt.com/after"))
	if f.dials.Load() != 1 {
		t.Fatal("stream cancellation closed the shared connection")
	}
}

func TestCodexHTTPPoolIsolationAndEviction(t *testing.T) {
	f := newPoolTestFixture(t, "h2", func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ok") })
	first := poolRouter(t, f)
	second := &codexHTTPPoolRouter{account: [32]byte{2}, dial: f.dial, cache: first.cache}
	t.Cleanup(second.CloseIdleConnections)
	third := &codexHTTPPoolRouter{account: first.account, proxyURL: "socks5://other:1080", dial: f.dial, cache: first.cache}
	t.Cleanup(third.CloseIdleConnections)
	for _, r := range []*codexHTTPPoolRouter{first, second, third} {
		readPoolReply(t, poolGET(t, r, t.Context(), "https://chatgpt.com/responses"))
	}
	readPoolReply(t, poolGET(t, first, t.Context(), "https://chatgpt.com:444/responses"))
	readPoolReply(t, poolGET(t, first, withTLSHandshakeTimeout(t.Context(), time.Second), "https://chatgpt.com/responses"))
	if f.dials.Load() != 5 {
		t.Fatalf("isolation handshakes=%d", f.dials.Load())
	}
	// A fresh client/attempt must reuse the same account/route pool.
	fresh := &codexHTTPPoolRouter{account: first.account, dial: f.dial, cache: first.cache}
	readPoolReply(t, poolGET(t, fresh, t.Context(), "https://chatgpt.com/responses"))
	if f.dials.Load() != 5 {
		t.Fatal("new client did not share transport")
	}
	bounded := poolRouter(t, f)
	bounded.cache = newCodexHTTPPoolCache(1)
	readPoolReply(t, poolGET(t, bounded, t.Context(), "https://chatgpt.com:445/a"))
	f.mu.Lock()
	old := f.connections[len(f.connections)-1]
	f.mu.Unlock()
	readPoolReply(t, poolGET(t, bounded, t.Context(), "https://chatgpt.com:446/a"))
	awaitPoolClosed(t, old)
	if bounded.cache.Len() != 1 {
		t.Fatal("pool bound exceeded")
	}
}

func TestCodexHTTPPoolEvictionPreservesActiveResponse(t *testing.T) {
	for _, protocol := range []string{"h2", "http/1.1"} {
		t.Run(protocol, func(t *testing.T) {
			release := make(chan struct{})
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			f := newPoolTestFixture(t, protocol, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/hold" {
					_, _ = io.WriteString(w, "start")
					w.(http.Flusher).Flush()
					<-release
					_, _ = io.WriteString(w, "finish")
					return
				}
				_, _ = io.WriteString(w, "other")
			})
			r := poolRouter(t, f)
			r.cache = newCodexHTTPPoolCache(1)
			held := poolGET(t, r, t.Context(), "https://chatgpt.com/hold")
			f.mu.Lock()
			old := f.connections[0]
			f.mu.Unlock()
			readPoolReply(t, poolGET(t, r, t.Context(), "https://chatgpt.com:444/other"))
			select {
			case <-old.closed:
				t.Fatal("eviction closed an active response")
			default:
			}
			close(release)
			if readPoolReply(t, held) != "startfinish" {
				t.Fatal("response truncated")
			}
			awaitPoolClosed(t, old)
		})
	}
}

func TestCodexHTTPPoolRecoversClosedConnection(t *testing.T) {
	f := newPoolTestFixture(t, "http/1.1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Connection", "close")
		_, _ = io.WriteString(w, "ok")
	})
	r := poolRouter(t, f)
	for range 2 {
		readPoolReply(t, poolGET(t, r, t.Context(), "https://chatgpt.com/a"))
	}
	if f.dials.Load() != 2 {
		t.Fatal("closed connection reused")
	}
}

func TestCodexHTTPPoolCancelledWaiter(t *testing.T) {
	started := make(chan struct{})
	p := &codexHTTPPool{dial: func(ctx context.Context, _, _ string) (net.Conn, string, error) {
		close(started)
		<-ctx.Done()
		return nil, "", ctx.Err()
	}}
	first, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := p.getTransport(first); done <- err }()
	<-started
	waiter, stop := context.WithCancel(t.Context())
	stop()
	if _, err := p.getTransport(waiter); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled waiter: %v", err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("dial cancellation: %v", err)
	}
}

func TestCodexHTTPPoolFactoryKeepsOverridesAndOtherProviders(t *testing.T) {
	a := &auth.Auth{ID: t.Name(), Provider: "codex"}
	client := NewCodexHTTPClient(t.Context(), nil, a, 0)
	fallback := client.Transport.(*codexResponseTransport).next.(*fallbackRoundTripper)
	if _, ok := fallback.chrome.(*codexHTTPPoolRouter); !ok {
		t.Fatal("native OAuth not pooled")
	}
	other := NewUtlsHTTPClient(t.Context(), nil, a, 0)
	if _, ok := other.Transport.(*fallbackRoundTripper).chrome.(*utlsRoundTripper); !ok {
		t.Fatal("generic provider transport changed")
	}
	custom := utlsClientRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("override"))}, nil
	})
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(custom))
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://chatgpt.com/a", nil)
	response, err := NewCodexHTTPClient(ctx, &config.Config{}, a, 0).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if readPoolReply(t, response) != "override" {
		t.Fatal("SDK override bypassed")
	}
}

func TestCodexHTTPPoolConcurrentColdStart(t *testing.T) {
	f := newPoolTestFixture(t, "h2", func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ok") })
	r := poolRouter(t, f)
	start := make(chan struct{})
	results := make(chan error, 16)
	for range 16 {
		go func() {
			<-start
			request, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://chatgpt.com/responses", nil)
			response, err := r.RoundTrip(request)
			if err == nil {
				_, err = io.ReadAll(response.Body)
				_ = response.Body.Close()
			}
			results <- err
		}()
	}
	close(start)
	for range 16 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if f.dials.Load() != 1 {
		t.Fatalf("concurrent initial handshakes=%d", f.dials.Load())
	}
}

func TestCodexHTTPPoolALPNChangeInvalidatesTransport(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ok") }
	h1 := newPoolTestFixture(t, "http/1.1", handler)
	h2 := newPoolTestFixture(t, "h2", handler)
	var upgraded atomic.Bool
	r := poolRouter(t, h1)
	r.dial = func(ctx context.Context, host, addr string) (net.Conn, string, error) {
		if upgraded.Load() {
			return h2.dial(ctx, host, addr)
		}
		return h1.dial(ctx, host, addr)
	}
	readPoolReply(t, poolGET(t, r, t.Context(), "https://chatgpt.com/a"))
	r.CloseIdleConnections()
	upgraded.Store(true)
	request, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://chatgpt.com/a", nil)
	if response, err := r.RoundTrip(request); !errors.Is(err, errCodexALPNChanged) {
		if response != nil {
			_ = response.Body.Close()
		}
		t.Fatalf("expected protocol change, got %v", err)
	}
	if r.cache.Len() != 0 {
		t.Fatal("old protocol still cached")
	}
	response := poolGET(t, r, t.Context(), "https://chatgpt.com/a")
	if response.ProtoMajor != 2 {
		t.Fatal("new attempt did not renegotiate HTTP/2")
	}
	readPoolReply(t, response)
}
