package helps

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/proxyutil"
	"golang.org/x/net/publicsuffix"
)

// Match openai/codex 17a0e6f63f http-client/chatgpt_cloudflare_cookies.rs.
// Unlike the CLI's process-global jar, CPA separates accounts and proxy routes.
const (
	codexCookieScopes    = 256
	codexCookiesPerScope = 64
	codexCookieMaxBytes  = 4096
	codexCookieIdleTTL   = 24 * time.Hour
)

type codexCookiePartition struct {
	jar   *cookiejar.Jar
	slots map[string]struct{}
	used  time.Time
}

type codexCookieCache struct {
	mu         sync.Mutex
	partitions map[[32]byte]*codexCookiePartition
	now        func() time.Time
}

var codexCookies = &codexCookieCache{partitions: make(map[[32]byte]*codexCookiePartition), now: time.Now}

// CodexCookieStore is an attempt-scoped handle. It contains no cookie values;
// those live only in a bounded account/egress partition, never on disk.
type CodexCookieStore struct {
	account [32]byte
	proxy   proxyutil.Setting
	cache   *codexCookieCache
}

func NewCodexCookieStore(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth) *CodexCookieStore {
	if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") || (auth.AuthKind() == cliproxyauth.AuthKindAPIKey || strings.TrimSpace(auth.Attributes["api_key"]) != "") {
		return nil
	}
	values := []string{strings.TrimSpace(auth.ID)}
	for _, key := range []string{"account_id", "chatgpt_account_id", "chatgpt_user_id"} {
		value, _ := auth.Metadata[key].(string)
		values = append(values, strings.TrimSpace(value))
	}
	if strings.Join(values, "") == "" {
		return nil
	}
	setting, err := proxyutil.Parse(effectiveProxyURL(ctx, cfg, auth))
	if err != nil {
		return nil
	}
	raw, _ := json.Marshal(values)
	return &CodexCookieStore{account: sha256.Sum256(raw), proxy: setting, cache: codexCookies}
}

func codexCookieURL(raw string) *url.URL {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "wss") {
		return nil
	}
	host := strings.ToLower(u.Hostname())
	if host != "chatgpt.com" && host != "chat.openai.com" && host != "chatgpt-staging.com" && !strings.HasSuffix(host, ".chatgpt.com") && !strings.HasSuffix(host, ".chatgpt-staging.com") {
		return nil
	}
	u.Scheme = "https"
	return u
}

func allowedCodexCookie(name string) bool {
	switch name {
	case "__cf_bm", "__cflb", "__cfruid", "__cfseq", "__cfwaitingroom", "__oailb", "_cfuvid", "cf_clearance", "cf_ob_info", "cf_use_ob":
		return true
	default:
		return strings.HasPrefix(name, "cf_chl_")
	}
}

func (s *CodexCookieStore) key(raw string, u *url.URL) [32]byte {
	route := "direct"
	if s.proxy.Mode == proxyutil.ModeProxy {
		route = s.proxy.URL.String()
	} else if s.proxy.Mode == proxyutil.ModeInherit && (strings.HasPrefix(raw, "wss:") || !strings.EqualFold(u.Hostname(), "chatgpt.com")) {
		// HTTP to chatgpt.com uses the direct uTLS dialer when no proxy is
		// configured. Other HTTP hosts and WS honor ProxyFromEnvironment.
		if p, err := http.ProxyFromEnvironment(&http.Request{URL: u}); err == nil && p != nil {
			route = p.String()
		}
	}
	return sha256.Sum256(append(append([]byte(nil), s.account[:]...), []byte(route)...))
}

// partition is called with the cache lock held. Empty reads never allocate jars.
func (c *codexCookieCache) partition(key [32]byte, create bool) *codexCookiePartition {
	now := c.now()
	for k, p := range c.partitions {
		if now.Sub(p.used) >= codexCookieIdleTTL {
			delete(c.partitions, k)
		}
	}
	p := c.partitions[key]
	if p == nil && create {
		if len(c.partitions) >= codexCookieScopes {
			var oldest [32]byte
			var timestamp time.Time
			for k, entry := range c.partitions {
				if timestamp.IsZero() || entry.used.Before(timestamp) {
					oldest, timestamp = k, entry.used
				}
			}
			delete(c.partitions, oldest)
		}
		jar, _ := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
		p = &codexCookiePartition{jar: jar, slots: make(map[string]struct{})}
		c.partitions[key] = p
	}
	if p != nil {
		p.used = now
	}
	return p
}

// AddHeader respects explicit Cookie headers, including an explicitly empty one.
func (s *CodexCookieStore) AddHeader(rawURL string, headers http.Header) {
	if s == nil || headers == nil {
		return
	}
	u := codexCookieURL(rawURL)
	if u == nil {
		return
	}
	for name := range headers {
		if strings.EqualFold(name, "Cookie") {
			return
		}
	}
	key := s.key(rawURL, u)
	s.cache.mu.Lock()
	defer s.cache.mu.Unlock()
	p := s.cache.partition(key, false)
	if p == nil {
		return
	}
	cookies := p.jar.Cookies(u)
	values := make([]string, 0, len(cookies))
	for _, cookie := range cookies {
		if allowedCodexCookie(cookie.Name) {
			values = append(values, cookie.String())
		}
	}
	if len(values) > 0 {
		headers.Set("Cookie", strings.Join(values, "; "))
	}
}

// StoreResponse also accepts rejected WS upgrades and non-2xx HTTP responses.
func (s *CodexCookieStore) StoreResponse(rawURL string, response *http.Response) {
	if s == nil || response == nil {
		return
	}
	u := codexCookieURL(rawURL)
	if u == nil {
		return
	}
	var cookies []*http.Cookie
	for _, line := range response.Header.Values("Set-Cookie") {
		if len(line) > codexCookieMaxBytes {
			continue
		}
		cookie, err := http.ParseSetCookie(line)
		if err == nil && allowedCodexCookie(cookie.Name) && cookie.Valid() == nil {
			cookies = append(cookies, cookie)
		}
	}
	if len(cookies) == 0 {
		return
	}
	key := s.key(rawURL, u)
	s.cache.mu.Lock()
	defer s.cache.mu.Unlock()
	p := s.cache.partition(key, true)
	now := s.cache.now()
	for _, cookie := range cookies {
		domain := strings.ToLower(strings.TrimPrefix(cookie.Domain, "."))
		if domain == "" {
			domain = strings.ToLower(u.Hostname())
		}
		host := strings.ToLower(u.Hostname())
		if host != domain && !strings.HasSuffix(host, "."+domain) {
			continue
		}
		if suffix, _ := publicsuffix.PublicSuffix(domain); suffix == domain && host != domain {
			continue
		}
		path := cookie.Path
		if path == "" || path[0] != '/' {
			path = u.Path
			if i := strings.LastIndex(path, "/"); i > 0 {
				path = path[:i]
			} else {
				path = "/"
			}
		}
		slot := domain + "\x00" + path + "\x00" + cookie.Name
		if cookie.MaxAge < 0 || (cookie.MaxAge == 0 && !cookie.Expires.IsZero() && !cookie.Expires.After(now)) {
			p.jar.SetCookies(u, []*http.Cookie{cookie})
			delete(p.slots, slot)
			continue
		}
		if _, exists := p.slots[slot]; !exists && len(p.slots) >= codexCookiesPerScope {
			continue
		}
		p.jar.SetCookies(u, []*http.Cookie{cookie})
		// Keep admission slots until explicit deletion or partition eviction.
		// The standard jar independently enforces expiry; retaining slots avoids
		// clock drift or huge Max-Age values bypassing the memory bound.
		p.slots[slot] = struct{}{}
	}
}
