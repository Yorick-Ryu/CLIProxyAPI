package helps

import (
	"context"
	"net"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/httpwire"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/proxyutil"
	"golang.org/x/net/proxy"
)

// CodexManagementQuery limits compatibility to the read-only ChatGPT queries.
// Check every RoundTrip so redirects cannot apply it to unrelated destinations.
func CodexManagementQuery(r *http.Request) bool {
	if r == nil || r.URL == nil || r.Method != http.MethodGet || r.URL.Scheme != "https" || !strings.EqualFold(r.URL.Hostname(), "chatgpt.com") {
		return false
	}
	switch r.URL.EscapedPath() {
	case "/backend-api/wham/usage", "/backend-api/subscriptions", "/backend-api/wham/rate-limit-reset-credits":
		return true
	}
	return false
}

type codexManagementTransport struct {
	linux    *http.Transport
	fallback *http.Transport
}

func (t *codexManagementTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if CodexManagementQuery(r) {
		return t.linux.RoundTrip(r)
	}
	return t.fallback.RoundTrip(r)
}
func (t *codexManagementTransport) CloseIdleConnections() {
	t.linux.CloseIdleConnections()
	t.fallback.CloseIdleConnections()
}

// NewCodexManagementTransport applies wire compatibility without changing caller
// headers, credentials, body, cookies or proxy precedence. Invalid proxies return
// nil so the management caller can retain its existing fallback policy.
func NewCodexManagementTransport(proxyURL string) http.RoundTripper {
	fallback, _, err := proxyutil.BuildHTTPTransport(proxyURL)
	if err != nil {
		return nil
	}
	if fallback == nil {
		fallback = http.DefaultTransport.(*http.Transport).Clone()
		fallback.Proxy = nil
	}
	dialer, _, err := proxyutil.BuildDialer(proxyURL)
	if err != nil {
		return nil
	}
	if dialer == nil {
		dialer = proxy.Direct
	}
	u := &utlsRoundTripper{codexLinux: true, dialer: dialer}
	transport := &http.Transport{DisableCompression: true,
		DialTLSContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			conn, err := u.createConnection(ctx, host, addr)
			if err != nil {
				return nil, err
			}
			return httpwire.NewOrderedRequestConnWithNames(conn, codexManagementHeaderOrder), nil
		},
	}
	return &codexManagementTransport{linux: transport, fallback: fallback}
}

// Management queries have no Responses session-header template. Preserve their
// observed CLI query groups, retain unknown fields and place framing last.
func codexManagementHeaderOrder(_, _ string, names []string) []string {
	return codexHeaderOrder(names, []string{"user-agent", "authorization", "chatgpt-account-id", "accept"}, []string{"host", "content-length", "transfer-encoding"})
}
