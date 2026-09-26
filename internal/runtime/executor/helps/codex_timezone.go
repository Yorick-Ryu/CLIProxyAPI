package helps

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"golang.org/x/sync/singleflight"
)

var environmentBlock = regexp.MustCompile(`(?s)^\s*<environment_context>[^<]*(?:<[^>]+>[^<]*)*</environment_context>\s*$`)
var timezoneTag = regexp.MustCompile(`<timezone>[^<>]*</timezone>`)
var dateTag = regexp.MustCompile(`<current_date>[^<>]*</current_date>`)

// environmentTexts selects only standalone user environment blocks, never tool output or instructions.
func environmentTexts(body []byte) map[string]string {
	out := make(map[string]string)
	if !gjson.ValidBytes(body) {
		return out
	}
	for i, item := range gjson.GetBytes(body, "input").Array() {
		if item.Get("role").String() != "user" {
			continue
		}
		kind := item.Get("type").String()
		if kind != "" && kind != "message" {
			continue
		}
		content := item.Get("content")
		prefix := fmt.Sprintf("input.%d.content", i)
		add := func(path string, value gjson.Result) {
			if value.Type == gjson.String && environmentBlock.MatchString(value.Str) && timezoneTag.MatchString(value.Str) {
				out[path] = value.Str
			}
		}
		if content.Type == gjson.String {
			add(prefix, content)
			continue
		}
		for j, part := range content.Array() {
			if part.Get("type").String() == "input_text" {
				add(fmt.Sprintf("%s.%d.text", prefix, j), part.Get("text"))
			}
		}
	}
	return out
}

// rewriteTimezone changes only selected string values and preserves all other JSON fields.
func rewriteTimezone(body []byte, texts map[string]string, zone string, now time.Time) []byte {
	loc, err := time.LoadLocation(zone)
	if err != nil || zone == "" || zone == "Local" {
		return body
	}
	out := body
	for path, text := range texts {
		next := timezoneTag.ReplaceAllString(text, "<timezone>"+zone+"</timezone>")
		next = dateTag.ReplaceAllString(next, "<current_date>"+now.In(loc).Format("2006-01-02")+"</current_date>")
		if next == text {
			continue
		}
		out, err = sjson.SetBytes(out, path, next)
		if err != nil {
			return body
		}
	}
	return out
}

// ApplyCodexTimezone is shared by HTTP, compact, and WebSocket request preparation.
// Lookup failures are deliberately fail-open and no credential is sent to the GeoIP service.
func ApplyCodexTimezone(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, body []byte) []byte {
	if cfg == nil {
		return body
	}
	policy := cfg.Codex.Timezone
	if auth != nil {
		override, err := config.CodexTimezoneFromMetadata(auth.Metadata)
		if err != nil {
			return body
		}
		if override != nil {
			policy = *override
		}
	}
	if policy.Mode == "" || policy.Mode == "off" || policy.Validate() != nil {
		return body
	}
	texts := environmentTexts(body)
	if len(texts) == 0 {
		return body
	}
	zone := policy.Zone
	if policy.Mode == "egress" {
		zone = codexTimezoneCache.resolve(ctx, cfg, auth)
	}
	return rewriteTimezone(body, texts, zone, time.Now())
}

type timezoneEntry struct {
	zone  string
	until time.Time
}
type timezoneResolver struct {
	mu      sync.Mutex
	entries map[[32]byte]timezoneEntry
	group   singleflight.Group
	now     func() time.Time
	lookup  func(context.Context, *config.Config, *cliproxyauth.Auth) string
}

var codexTimezoneCache = &timezoneResolver{entries: make(map[[32]byte]timezoneEntry), now: time.Now, lookup: lookupEgressTimezone}

func (r *timezoneResolver) resolve(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth) string {
	id := ""
	if auth != nil {
		id = auth.ID
	}
	// Include injected transport identity so different execution routes cannot share a result.
	key := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%p", id, effectiveProxyURL(ctx, cfg, auth), ctx.Value("cliproxy.roundtripper"))))
	get := func() (string, bool) {
		r.mu.Lock()
		defer r.mu.Unlock()
		e, ok := r.entries[key]
		return e.zone, ok && r.now().Before(e.until)
	}
	if zone, ok := get(); ok {
		return zone
	}
	ch := r.group.DoChan(fmt.Sprintf("%x", key), func() (any, error) {
		if zone, ok := get(); ok {
			return zone, nil
		}
		zone := r.lookup(ctx, cfg, auth)
		ttl := 6 * time.Hour
		if zone == "" {
			ttl = time.Minute
		}
		r.mu.Lock()
		if len(r.entries) >= 1024 {
			for k, e := range r.entries {
				if !r.now().Before(e.until) {
					delete(r.entries, k)
				}
			}
			if len(r.entries) >= 1024 {
				for k := range r.entries {
					delete(r.entries, k)
					break
				}
			}
		}
		r.entries[key] = timezoneEntry{zone: zone, until: r.now().Add(ttl)}
		r.mu.Unlock()
		return zone, nil
	})
	select {
	case <-ctx.Done():
		return ""
	case result := <-ch:
		zone, _ := result.Val.(string)
		return zone
	}
}

func lookupEgressTimezone(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth) string {
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	proxy := effectiveProxyURL(ctx, cfg, auth)
	if proxy != "" {
		transport, _, err := proxyutil.BuildHTTPTransport(proxy)
		if err != nil || transport == nil {
			return ""
		}
		defer transport.CloseIdleConnections()
		client.Transport = transport
	} else if rt, ok := ctx.Value("cliproxy.roundtripper").(http.RoundTripper); ok {
		client.Transport = rt
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://ipwho.is/?fields=success,timezone.id", nil)
	if err != nil {
		return ""
	}
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var result struct {
		Success  bool `json:"success"`
		Timezone struct {
			ID string `json:"id"`
		} `json:"timezone"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 16384)).Decode(&result) != nil || !result.Success {
		return ""
	}
	zone := strings.TrimSpace(result.Timezone.ID)
	if zone == "" || zone == "Local" {
		return ""
	}
	if _, err = time.LoadLocation(zone); err != nil {
		return ""
	}
	return zone
}
