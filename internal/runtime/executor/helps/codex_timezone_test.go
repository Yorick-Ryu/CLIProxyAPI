package helps

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func timezoneBody(t *testing.T, text string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"unknown": map[string]any{"keep": true}, "input": []any{map[string]any{"type": "message", "id": "keep-id", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": text}}}}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func TestCodexTimezoneRewrite(t *testing.T) {
	text := "<environment_context>\n<current_date>2026-09-26</current_date>\n<timezone>Asia/Shanghai</timezone>\n<cwd>/work</cwd>\n</environment_context>"
	body := timezoneBody(t, text)
	now := time.Date(2026, 9, 26, 1, 0, 0, 0, time.UTC)
	out := rewriteTimezone(body, environmentTexts(body), "Pacific/Honolulu", now)
	got := gjson.GetBytes(out, "input.0.content.0.text").String()
	if !strings.Contains(got, "<timezone>Pacific/Honolulu</timezone>") || !strings.Contains(got, "<current_date>2026-09-25</current_date>") || !strings.Contains(got, "<cwd>/work</cwd>") {
		t.Fatal(got)
	}
	if !gjson.GetBytes(out, "unknown.keep").Bool() || gjson.GetBytes(out, "input.0.id").String() != "keep-id" {
		t.Fatal("lost unknown fields")
	}
	for _, mode := range []string{"", "off", "invalid"} {
		cfg := &config.Config{}
		cfg.Codex.Timezone.Mode = mode
		if string(ApplyCodexTimezone(context.Background(), cfg, nil, body)) != string(body) {
			t.Fatalf("mode %q changed bytes", mode)
		}
	}
	for _, text := range []string{"quoted: " + text, "```xml\n" + text + "\n```", "<environment_context><cwd>/work</cwd></environment_context>"} {
		b := timezoneBody(t, text)
		if len(environmentTexts(b)) != 0 {
			t.Fatal("matched non-environment or absent timezone")
		}
	}
	cfg := &config.Config{}
	cfg.Codex.Timezone = config.CodexTimezoneConfig{Mode: "fixed", Zone: "Europe/Berlin"}
	for _, b := range [][]byte{[]byte(`{broken`), []byte(`{"input":[{"role":"assistant","content":"<environment_context><timezone>Asia/Shanghai</timezone></environment_context>"}]}`)} {
		if string(ApplyCodexTimezone(context.Background(), cfg, nil, b)) != string(b) {
			t.Fatal("changed invalid/unrelated body")
		}
	}
	b := []byte(`{"input":[{"role":"user","content":"<environment_context><timezone>Asia/Shanghai</timezone></environment_context>"}]}`)
	if len(environmentTexts(b)) != 1 {
		t.Fatal("string content not selected")
	}
	out = rewriteTimezone(b, environmentTexts(b), "Asia/Shanghai", now)
	if string(out) != string(b) {
		t.Fatal("unchanged body reserialized")
	}
}

func TestCodexTimezoneCacheExpiryAndIsolation(t *testing.T) {
	now := time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)
	calls := 0
	r := &timezoneResolver{entries: make(map[[32]byte]timezoneEntry), now: func() time.Time { return now }, lookup: func(context.Context, *config.Config, *cliproxyauth.Auth) string { calls++; return "Asia/Tokyo" }}
	cfg := &config.Config{}
	a := &cliproxyauth.Auth{ID: "a", ProxyURL: "socks5://proxy:1080"}
	ctx := context.Background()
	if r.resolve(ctx, cfg, a) != "Asia/Tokyo" || r.resolve(ctx, cfg, a) != "Asia/Tokyo" || calls != 1 {
		t.Fatal("cache miss")
	}
	a.ProxyURL = "socks5://other:1080"
	r.resolve(ctx, cfg, a)
	if calls != 2 {
		t.Fatal("proxy change reused cache")
	}
	now = now.Add(7 * time.Hour)
	r.resolve(ctx, cfg, a)
	if calls != 3 {
		t.Fatal("expiry ignored")
	}
	r.lookup = func(context.Context, *config.Config, *cliproxyauth.Auth) string { calls++; return "" }
	now = now.Add(7 * time.Hour)
	if r.resolve(ctx, cfg, a) != "" || r.resolve(ctx, cfg, a) != "" || calls != 4 {
		t.Fatal("negative cache")
	}
}

type timezoneRoundTripper func(*http.Request) (*http.Response, error)

func (f timezoneRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestEgressTimezoneLookupUsesRouteWithoutCredentials(t *testing.T) {
	calls := 0
	rt := timezoneRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Host != "ipwho.is" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Fatal("wrong target or leaked credentials")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"success":true,"timezone":{"id":"Asia/Tokyo"}}`))}, nil
	})
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(rt))
	if lookupEgressTimezone(ctx, &config.Config{}, nil) != "Asia/Tokyo" || calls != 1 {
		t.Fatal("context transport ignored")
	}
	if lookupEgressTimezone(ctx, &config.Config{}, &cliproxyauth.Auth{ProxyURL: "invalid://secret"}) != "" || calls != 1 {
		t.Fatal("invalid proxy fell back to direct")
	}
}

func TestCodexTimezoneEgressFailurePreservesBody(t *testing.T) {
	previous := codexTimezoneCache
	t.Cleanup(func() { codexTimezoneCache = previous })
	calls := 0
	codexTimezoneCache = &timezoneResolver{entries: make(map[[32]byte]timezoneEntry), now: time.Now, lookup: func(context.Context, *config.Config, *cliproxyauth.Auth) string { calls++; return "" }}
	cfg := &config.Config{}
	cfg.Codex.Timezone.Mode = "egress"
	body := timezoneBody(t, "<environment_context><timezone>Asia/Shanghai</timezone></environment_context>")
	if string(ApplyCodexTimezone(context.Background(), cfg, nil, body)) != string(body) || calls != 1 {
		t.Fatal("lookup failure changed body")
	}
	cfg.Codex.Timezone.Mode = "off"
	ApplyCodexTimezone(context.Background(), cfg, nil, body)
	if calls != 1 {
		t.Fatal("disabled mode did lookup")
	}
}

func TestCodexTimezoneCanceledLookupDoesNotBlockCaller(t *testing.T) {
	started := make(chan struct{})
	finished := make(chan struct{})
	r := &timezoneResolver{entries: make(map[[32]byte]timezoneEntry), now: time.Now, lookup: func(ctx context.Context, _ *config.Config, _ *cliproxyauth.Auth) string {
		close(started)
		<-ctx.Done()
		close(finished)
		return ""
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan string, 1)
	go func() { result <- r.resolve(ctx, &config.Config{}, nil) }()
	<-started
	cancel()
	if <-result != "" {
		t.Fatal("canceled lookup returned zone")
	}
	<-finished
}

func TestCodexTimezoneAccountOverrides(t *testing.T) {
	previous := codexTimezoneCache
	t.Cleanup(func() { codexTimezoneCache = previous })
	calls := 0
	codexTimezoneCache = &timezoneResolver{entries: make(map[[32]byte]timezoneEntry), now: time.Now,
		lookup: func(context.Context, *config.Config, *cliproxyauth.Auth) string { calls++; return "Pacific/Honolulu" }}
	cfg := &config.Config{Codex: config.CodexConfig{Timezone: config.CodexTimezoneConfig{Mode: "fixed", Zone: "Europe/Berlin"}}}
	body := timezoneBody(t, "<environment_context><timezone>Asia/Shanghai</timezone></environment_context>")
	check := func(mode, zone, want string) {
		t.Helper()
		auth := &cliproxyauth.Auth{ID: "account", Metadata: map[string]any{"codex_timezone_mode": mode, "codex_timezone": zone}}
		out := ApplyCodexTimezone(context.Background(), cfg, auth, body)
		text := gjson.GetBytes(out, "input.0.content.0.text").String()
		if !strings.Contains(text, "<timezone>"+want+"</timezone>") {
			t.Fatal(text)
		}
		if want == "Asia/Shanghai" && string(out) != string(body) {
			t.Fatal("off changed bytes")
		}
	}
	check("off", "", "Asia/Shanghai")
	check("fixed", "Asia/Tokyo", "Asia/Tokyo")
	check("egress", "", "Pacific/Honolulu")
	check("inherit", "", "Europe/Berlin")
	check("", "", "Europe/Berlin")
	check("fixed", "invalid/zone", "Asia/Shanghai")
	if calls != 1 {
		t.Fatal("unnecessary lookups")
	}
	cfg.Codex.Timezone.Mode = "off"
	check("fixed", "Asia/Tokyo", "Asia/Tokyo")
	check("inherit", "", "Asia/Shanghai")
}
