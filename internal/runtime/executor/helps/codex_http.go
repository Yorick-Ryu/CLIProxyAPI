package helps

import (
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// NewCodexHTTPClient preserves the native CLI's absent Accept-Encoding header.
// Gzip decoding remains available when the server actually returns gzip, without
// coupling this behavior to zstd compression of the outgoing request body.
func NewCodexHTTPClient(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, timeout time.Duration) *http.Client {
	client := NewUtlsHTTPClient(ctx, cfg, auth, timeout, true)
	client.Transport = &codexResponseTransport{next: client.Transport}
	return client
}

var codexDefaultHTTPTransport = sync.OnceValue(func() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableCompression = true
	return transport
})

func withoutAutomaticCompression(transport http.RoundTripper) http.RoundTripper {
	switch value := transport.(type) {
	case *utlsRoundTripper:
		copy := *value
		copy.disableCompression = true
		return &copy
	case *http.Transport:
		if value == http.DefaultTransport {
			// Preserve connection pooling for standard non-ChatGPT upstreams.
			return codexDefaultHTTPTransport()
		}
		copy := value.Clone()
		copy.DisableCompression = true
		return copy
	default:
		// Custom transports supplied by embedders retain their own behavior.
		return transport
	}
}

type codexResponseTransport struct{ next http.RoundTripper }

func (t *codexResponseTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.next.RoundTrip(request)
	if err != nil || response == nil || response.Body == nil || request.Method == http.MethodHead {
		return response, err
	}
	if strings.EqualFold(strings.TrimSpace(response.Header.Get("Content-Encoding")), "gzip") {
		response.Body = &codexGzipBody{source: response.Body}
		response.Header.Del("Content-Encoding")
		response.Header.Del("Content-Length")
		response.ContentLength = -1
		response.Uncompressed = true
	}
	return response, nil
}

func (t *codexResponseTransport) CloseIdleConnections() {
	if closer, ok := t.next.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

// Initialize lazily so response headers can be processed before stream data
// arrives. Closing an unread body still closes the upstream connection.
type codexGzipBody struct {
	source io.ReadCloser
	reader *gzip.Reader
	once   sync.Once
	err    error
}

func (b *codexGzipBody) Read(p []byte) (int, error) {
	b.once.Do(func() { b.reader, b.err = gzip.NewReader(b.source) })
	if b.err != nil {
		return 0, b.err
	}
	return b.reader.Read(p)
}

func (b *codexGzipBody) Close() error {
	// gzip.Reader.Close does not release the underlying body. Closing that body
	// is sufficient and can safely interrupt an in-flight Read.
	return b.source.Close()
}
