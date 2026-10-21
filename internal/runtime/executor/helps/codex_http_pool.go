package helps

import (
	"context"
	"crypto/sha256"
	stdtls "crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"

	internalcache "github.com/router-for-me/CLIProxyAPI/v8/internal/cache"
	"golang.org/x/net/http2"
)

const codexHTTPPoolCapacity = 256

var errCodexALPNChanged = errors.New("codex: negotiated protocol changed")

type codexTLSDial func(context.Context, string, string) (net.Conn, string, error)
type codexReusableTransport interface {
	http.RoundTripper
	CloseIdleConnections()
}

var codexHTTPPools = newCodexHTTPPoolCache(codexHTTPPoolCapacity)

func newCodexHTTPPoolCache(capacity int) *internalcache.BoundedLRU[[32]byte, *codexHTTPPool] {
	return internalcache.NewBoundedLRU(capacity, func(_ [32]byte, pool *codexHTTPPool) { pool.retire() })
}

// Like Codex's RouteAwareClientPool, retain transport clients across requests.
// CPA additionally partitions by auth identity, target and TLS policy. Only
// native ChatGPT HTTP traffic reaches this router; SDK overrides stay intact.
type codexHTTPPoolRouter struct {
	profile  string
	account  [32]byte
	proxyURL string
	dial     codexTLSDial
	cache    *internalcache.BoundedLRU[[32]byte, *codexHTTPPool]
	used     sync.Map
}

func (r *codexHTTPPoolRouter) RoundTrip(req *http.Request) (*http.Response, error) {
	host := strings.ToLower(req.URL.Hostname())
	port := req.URL.Port()
	if port == "" {
		port = "443"
	}
	addr := net.JoinHostPort(host, port)
	identity, _ := json.Marshal([]string{hex.EncodeToString(r.account[:]), r.proxyURL, addr, r.profile, tlsHandshakeTimeoutFromContext(req.Context()).String()})
	key := sha256.Sum256(identity)
	pool := r.cache.GetOrAdd(key, func() *codexHTTPPool { return &codexHTTPPool{dial: r.dial, host: host, addr: addr} })
	r.used.Range(func(key, value any) bool {
		p := value.(*codexHTTPPool)
		p.mu.Lock()
		retired := p.retired
		p.mu.Unlock()
		if retired {
			r.used.Delete(key)
		}
		return true
	})
	r.used.Store(key, pool)
	response, err := pool.RoundTrip(req)
	if errors.Is(err, errCodexALPNChanged) {
		// No HTTP bytes were sent on the changed-ALPN connection. Let the
		// executor own retries; the next attempt must negotiate afresh.
		r.cache.Delete(key)
	}
	return response, err
}

func (r *codexHTTPPoolRouter) CloseIdleConnections() {
	r.used.Range(func(key, value any) bool { value.(*codexHTTPPool).CloseIdleConnections(); return true })
}

// ALPN is selected once before handing the negotiated connection to the mature
// HTTP transports. The first connection is consumed by their normal dial hook;
// subsequent connections use exactly the same uTLS handshake and proxy route.
type codexHTTPPool struct {
	mu         sync.Mutex
	dialing    chan struct{}
	transport  codexReusableTransport
	first      net.Conn
	retired    bool
	dial       codexTLSDial
	host, addr string
}

func (p *codexHTTPPool) getTransport(ctx context.Context) (codexReusableTransport, error) {
	for {
		p.mu.Lock()
		if p.transport != nil {
			tr := p.transport
			p.mu.Unlock()
			return tr, nil
		}
		if pending := p.dialing; pending != nil {
			p.mu.Unlock()
			select {
			case <-pending:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		pending := make(chan struct{})
		p.dialing = pending
		p.mu.Unlock()
		conn, protocol, err := p.dial(ctx, p.host, p.addr)
		if err == nil && protocol != "h2" && protocol != "http/1.1" && protocol != "" {
			_ = conn.Close()
			err = fmt.Errorf("codex: unsupported ALPN %q", protocol)
		}
		p.mu.Lock()
		if err == nil {
			p.first = conn
			dial := func(ctx context.Context, _, _ string) (net.Conn, error) { return p.dialForProtocol(ctx, protocol) }
			if protocol == "h2" {
				p.transport = &http2.Transport{DisableCompression: true, StrictMaxConcurrentStreams: true, DialTLSContext: func(ctx context.Context, network, addr string, _ *stdtls.Config) (net.Conn, error) {
					return dial(ctx, network, addr)
				}}
			} else {
				// A custom TLS dialer returning uTLS is deliberately HTTP/1.1 here.
				// Do not add response/read/write deadlines to long-lived inference streams.
				p.transport = &http.Transport{DisableCompression: true, MaxIdleConns: 2, MaxIdleConnsPerHost: 2, DialTLSContext: dial}
			}
		}
		tr := p.transport
		p.dialing = nil
		close(pending)
		p.mu.Unlock()
		return tr, err
	}
}

func (p *codexHTTPPool) dialForProtocol(ctx context.Context, protocol string) (net.Conn, error) {
	p.mu.Lock()
	first := p.first
	p.first = nil
	p.mu.Unlock()
	if first != nil {
		return first, nil
	}
	conn, negotiated, err := p.dial(ctx, p.host, p.addr)
	if err != nil {
		return nil, err
	}
	if (protocol == "h2") != (negotiated == "h2") || (negotiated != "h2" && negotiated != "http/1.1" && negotiated != "") {
		_ = conn.Close()
		return nil, fmt.Errorf("%w from %q to %q", errCodexALPNChanged, protocol, negotiated)
	}
	return conn, nil
}

func (p *codexHTTPPool) RoundTrip(req *http.Request) (*http.Response, error) {
	tr, err := p.getTransport(req.Context())
	if err != nil {
		return nil, err
	}
	resp, err := tr.RoundTrip(req)
	if err != nil || resp == nil || resp.Body == nil {
		p.closeIfRetired()
		return resp, err
	}
	resp.Body = &codexPooledBody{ReadCloser: resp.Body, done: p.closeIfRetired}
	return resp, nil
}

func (p *codexHTTPPool) closeIfRetired() {
	p.mu.Lock()
	retired := p.retired
	p.mu.Unlock()
	if retired {
		p.CloseIdleConnections()
	}
}

func (p *codexHTTPPool) retire() {
	p.mu.Lock()
	p.retired = true
	p.mu.Unlock()
	p.CloseIdleConnections()
}

func (p *codexHTTPPool) CloseIdleConnections() {
	p.mu.Lock()
	tr := p.transport
	first := p.first
	p.first = nil
	p.mu.Unlock()
	if first != nil {
		_ = first.Close()
	}
	if tr != nil {
		tr.CloseIdleConnections()
	}
}

type codexPooledBody struct {
	io.ReadCloser
	done func()
}

func (b *codexPooledBody) Read(data []byte) (int, error) {
	n, err := b.ReadCloser.Read(data)
	if err != nil {
		b.done()
	}
	return n, err
}
func (b *codexPooledBody) Close() error {
	err := b.ReadCloser.Close()
	b.done()
	return err
}
