package helps

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	auth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	executor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func testCookieStore(t *testing.T) *CodexCookieStore {
	t.Helper()
	s := NewCodexCookieStore(t.Context(), nil, &auth.Auth{ID: t.Name(), Provider: "codex", ProxyURL: "direct"})
	if s == nil {
		t.Fatal("missing OAuth cookie store")
	}
	s.cache = &codexCookieCache{partitions: make(map[[32]byte]*codexCookiePartition), now: time.Now}
	return s
}
func cookieResponse(status int, lines ...string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Set-Cookie": lines}}
}
func cookieHeader(s *CodexCookieStore, u string) string {
	h := make(http.Header)
	s.AddHeader(u, h)
	return h.Get("Cookie")
}

func TestCodexCookiesHTTPWebSocketSharing(t *testing.T) {
	s := testCookieStore(t)
	const https = "https://cookies.chatgpt.com/backend-api/codex/responses"
	const wss = "wss://cookies.chatgpt.com/backend-api/codex/responses"
	s.StoreResponse(https, cookieResponse(200, "__oailb=from-http; Path=/; Secure; HttpOnly", "session=secret; Path=/"))
	if got := cookieHeader(s, wss); got != "__oailb=from-http" {
		t.Fatalf("WS cookies %q", got)
	}
	for _, status := range []int{101, 403, 429} {
		s.StoreResponse(wss, cookieResponse(status, fmt.Sprintf("__oailb=from-ws-%d; Path=/; Secure", status)))
		if got := cookieHeader(s, https); got != fmt.Sprintf("__oailb=from-ws-%d", status) {
			t.Fatalf("HTTP cookies %q", got)
		}
	}
	for _, h := range []http.Header{{"Cookie": {"explicit=owner"}}, {"cookie": {"explicit=owner"}}, {"Cookie": {}}} {
		before := fmt.Sprint(h)
		s.AddHeader(wss, h)
		if fmt.Sprint(h) != before {
			t.Fatal("explicit cookie overwritten")
		}
	}
}

func TestCodexCookiesOriginDomainPathAndExpiry(t *testing.T) {
	s := testCookieStore(t)
	const origin = "https://cookies.chatgpt.com/a/responses"
	for _, raw := range []string{"http://cookies.chatgpt.com/a", "ws://cookies.chatgpt.com/a", "https://api.openai.com/a", "https://chatgpt.com.evil.example/a", "https://evilchatgpt.com/a", "https://foo.chat.openai.com/a"} {
		s.StoreResponse(raw, cookieResponse(200, "__oailb=bad; Path=/"))
		if got := cookieHeader(s, raw); got != "" {
			t.Fatalf("disallowed origin %s", raw)
		}
	}
	if len(s.cache.partitions) != 0 {
		t.Fatal("disallowed origins allocated storage")
	}
	s.StoreResponse(origin, cookieResponse(200, "__oailb=scoped; Path=/a; Secure", "__cf_bm=bad; Domain=com; Path=/", "cf_clearance=bad; Domain=other.chatgpt.com; Path=/", "session=secret; Path=/"))
	if got := cookieHeader(s, origin); got != "__oailb=scoped" {
		t.Fatalf("unexpected cookies %q", got)
	}
	if cookieHeader(s, "https://cookies.chatgpt.com/b") != "" || cookieHeader(s, "https://other.chatgpt.com/a") != "" {
		t.Fatal("path or host scope escaped")
	}
	s.StoreResponse(origin, cookieResponse(200, "__oailb=; Max-Age=0; Path=/a; Secure"))
	if cookieHeader(s, origin) != "" {
		t.Fatal("deletion ignored")
	}
	s.StoreResponse(origin, cookieResponse(200, "__oailb=expired; Expires=Thu, 01 Jan 1970 00:00:01 GMT; Path=/a"))
	if cookieHeader(s, origin) != "" {
		t.Fatal("expired cookie returned")
	}
	s.StoreResponse(origin, cookieResponse(200, "cf_chl_test=\"quoted value\"; Domain=chatgpt.com; Path=/; Secure"))
	if got := cookieHeader(s, "wss://other.chatgpt.com/a"); got != `cf_chl_test="quoted value"` {
		t.Fatalf("domain/quoting: %q", got)
	}
}

func TestCodexCookiesAccountAndProxyIsolation(t *testing.T) {
	cfg := &config.Config{}
	a := &auth.Auth{ID: "one", Provider: "codex", Metadata: map[string]any{"account_id": "account-a"}, ProxyURL: "socks5://proxy:1080"}
	s := NewCodexCookieStore(t.Context(), cfg, a)
	s.cache = testCookieStore(t).cache
	const target = "https://chatgpt.com/backend-api/codex/responses"
	s.StoreResponse(target, cookieResponse(200, "__oailb=owner; Path=/; Secure"))
	same := NewCodexCookieStore(t.Context(), cfg, a)
	same.cache = s.cache
	if cookieHeader(same, target) != "__oailb=owner" {
		t.Fatal("new attempt lost cookies")
	}
	otherAccount := *a
	otherAccount.Metadata = map[string]any{"account_id": "account-b"}
	otherAuth := *a
	otherAuth.ID = "two"
	otherProxy := *a
	otherProxy.ProxyURL = "socks5://proxy:1081"
	for _, other := range []*auth.Auth{&otherAccount, &otherAuth, &otherProxy} {
		store := NewCodexCookieStore(t.Context(), cfg, other)
		store.cache = s.cache
		if cookieHeader(store, target) != "" {
			t.Fatal("cookie crossed account/egress boundary")
		}
	}
	override := NewCodexCookieStore(executor.WithRequestProxyURL(t.Context(), "socks5://proxy:1082"), cfg, a)
	override.cache = s.cache
	if cookieHeader(override, target) != "" {
		t.Fatal("execution proxy override reused cookies")
	}
	for _, other := range []*auth.Auth{nil, {Provider: "codex"}, {ID: "key", Provider: "codex", Attributes: map[string]string{auth.AttributeAuthKind: auth.AuthKindAPIKey}}, {ID: "claude", Provider: "claude"}} {
		if NewCodexCookieStore(t.Context(), cfg, other) != nil {
			t.Fatal("unexpected cookie store eligibility")
		}
	}
}

func TestCodexCookiesBoundedStorage(t *testing.T) {
	s := testCookieStore(t)
	const target = "https://chatgpt.com/responses"
	for i := 0; i < codexCookiesPerScope+10; i++ {
		s.StoreResponse(target, cookieResponse(200, fmt.Sprintf("cf_chl_%d=value; Path=/", i)))
	}
	key := s.key(target, codexCookieURL(target))
	p := s.cache.partitions[key]
	if len(p.slots) != codexCookiesPerScope || len(p.jar.Cookies(codexCookieURL(target))) != codexCookiesPerScope {
		t.Fatal("cookie capacity not enforced")
	}
	fresh := testCookieStore(t)
	fresh.StoreResponse(target, cookieResponse(200, "__oailb="+strings.Repeat("x", codexCookieMaxBytes)))
	if len(fresh.cache.partitions) != 0 {
		t.Fatal("oversized cookie allocated a jar")
	}
	if len(p.slots) != codexCookiesPerScope {
		t.Fatal("oversized cookie admitted")
	}
	now := time.Now()
	s.cache.now = func() time.Time { return now }
	for i := 0; i < codexCookieScopes; i++ {
		now = now.Add(time.Second)
		other := *s
		other.account = [32]byte{byte(i), 1}
		other.StoreResponse(target, cookieResponse(200, "__oailb=owner"))
	}
	if len(s.cache.partitions) != codexCookieScopes {
		t.Fatal("partition capacity not enforced")
	}
	if _, exists := s.cache.partitions[key]; exists {
		t.Fatal("oldest partition retained")
	}
	now = now.Add(codexCookieIdleTTL)
	if cookieHeader(s, target) != "" || len(s.cache.partitions) != 0 {
		t.Fatal("idle partitions retained")
	}
}

func TestCodexCookiesHTTPTransportPreservesRequestsAndErrors(t *testing.T) {
	a := &auth.Auth{ID: t.Name(), Provider: "codex"}
	const target = "https://cookie-http.chatgpt.com/responses"
	calls := 0
	rt := utlsClientRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 && r.Header.Get("Cookie") != "" {
			t.Fatal("unexpected initial cookie")
		}
		if calls == 2 && r.Header.Get("Cookie") != "__oailb=next" {
			t.Fatal("cookie not retained across HTTP attempts")
		}
		return &http.Response{StatusCode: 429, Header: http.Header{"Set-Cookie": {"__oailb=next; Secure; Path=/"}}, Body: io.NopCloser(strings.NewReader("limited")), Request: r}, nil
	})
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(rt))
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	for range 2 {
		client := NewCodexHTTPClient(ctx, nil, a, 0)
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if response.StatusCode != 429 || string(data) != "limited" {
			t.Fatal("response changed")
		}
		if request.Header.Get("Cookie") != "" {
			t.Fatal("caller request mutated")
		}
	}
}

func TestCodexCookiesConcurrentAccess(t *testing.T) {
	s := testCookieStore(t)
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			for range 20 {
				s.StoreResponse("https://chatgpt.com/a", cookieResponse(200, "__oailb=shared; Path=/"))
				_ = cookieHeader(s, "wss://chatgpt.com/a")
			}
		})
	}
	wg.Wait()
	if cookieHeader(s, "https://chatgpt.com/a") != "__oailb=shared" {
		t.Fatal("shared cookie lost")
	}
}

func TestCodexCookiesInheritedProxyRoutes(t *testing.T) {
	if os.Getenv("CPA_COOKIE_PROXY_CHILD") != "1" {
		binary, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(binary, "-test.run=^TestCodexCookiesInheritedProxyRoutes$")
		for _, value := range os.Environ() {
			name, _, _ := strings.Cut(value, "=")
			switch strings.ToLower(name) {
			case "http_proxy", "https_proxy", "no_proxy", "all_proxy", "cpa_cookie_proxy_child":
				continue
			}
			cmd.Env = append(cmd.Env, value)
		}
		cmd.Env = append(cmd.Env, "CPA_COOKIE_PROXY_CHILD=1", "HTTPS_PROXY=http://test-proxy:3128", "NO_PROXY=")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("proxy route test failed: %v\n%s", err, output)
		}
		return
	}
	a := &auth.Auth{ID: "route-account", Provider: "codex"}
	inherited := NewCodexCookieStore(t.Context(), nil, a)
	a.ProxyURL = "direct"
	direct := NewCodexCookieStore(t.Context(), nil, a)
	a.ProxyURL = "http://test-proxy:3128"
	proxied := NewCodexCookieStore(t.Context(), nil, a)
	for _, raw := range []string{"https://chatgpt.com/responses", "wss://chatgpt.com/responses", "https://chat.openai.com/responses"} {
		expected := proxied
		if raw == "https://chatgpt.com/responses" {
			expected = direct
		}
		u := codexCookieURL(raw)
		if inherited.key(raw, u) != expected.key(raw, u) {
			t.Fatalf("wrong route isolation for %s", raw)
		}
	}
}
