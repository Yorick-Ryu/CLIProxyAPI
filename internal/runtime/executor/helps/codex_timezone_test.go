package helps

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
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

func timezoneTestClock(t *testing.T, instant string) {
	t.Helper()
	now, err := time.Parse(time.RFC3339, instant)
	if err != nil {
		t.Fatal(err)
	}
	previous := codexEnvironmentNow
	codexEnvironmentNow = func() time.Time { return now }
	t.Cleanup(func() { codexEnvironmentNow = previous })
}

func TestCodexTimezoneLocalDateAndTimezone(t *testing.T) {
	timezoneTestClock(t, "2026-01-01T07:30:00Z")
	t.Setenv("TZ", "America/Los_Angeles")
	cfg := &config.Config{Codex: config.CodexConfig{Timezone: config.CodexTimezoneConfig{Mode: "local"}}}
	text := "<environment_context>\n<current_date>2026-01-01</current_date>\n<timezone>Asia/Shanghai</timezone>\n<cwd>/work</cwd>\n</environment_context>"
	body := timezoneBody(t, text)
	out := ApplyCodexTimezone(context.Background(), cfg, nil, body)
	got := gjson.GetBytes(out, "input.0.content.0.text").String()
	want := strings.ReplaceAll(strings.ReplaceAll(text, "2026-01-01", "2025-12-31"), "Asia/Shanghai", "America/Los_Angeles")
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if !gjson.GetBytes(out, "unknown.keep").Bool() || gjson.GetBytes(out, "input.0.id").String() != "keep-id" {
		t.Fatal("lost unrelated fields")
	}
	if string(body) == string(out) {
		t.Fatal("environment not updated")
	}
	again := ApplyCodexTimezone(context.Background(), cfg, nil, out)
	if string(again) != string(out) {
		t.Fatal("same clock rewrite is not idempotent")
	}
	for _, mode := range []string{"", "off", "fixed", "invalid"} {
		cfg.Codex.Timezone.Mode = mode
		if string(ApplyCodexTimezone(context.Background(), cfg, nil, body)) != string(body) {
			t.Fatalf("mode %q changed bytes", mode)
		}
	}
	if string(ApplyCodexTimezone(context.Background(), nil, nil, body)) != string(body) {
		t.Fatal("nil configuration changed bytes")
	}
}

func TestCodexTimezoneOnlyLatestEnvironment(t *testing.T) {
	timezoneTestClock(t, "2026-01-01T07:30:00Z")
	t.Setenv("TZ", "America/Los_Angeles")
	cfg := &config.Config{Codex: config.CodexConfig{Timezone: config.CodexTimezoneConfig{Mode: "local"}}}
	old := "<environment_context><current_date>2001-01-01</current_date><timezone>Asia/Shanghai</timezone></environment_context>"
	latest := "<environment_context>\n  <current_date status=\"unavailable\" />\n  <timezone>Etc/UTC</timezone>\n</environment_context>"
	input := []any{map[string]any{"role": "user", "content": old}, map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": latest}}}, map[string]any{"role": "assistant", "content": old}, map[string]any{"type": "function_call_output", "output": old}, map[string]any{"role": "user", "content": "Please explain: " + old}}
	b, err := json.Marshal(map[string]any{"input": input})
	if err != nil {
		t.Fatal(err)
	}
	out := ApplyCodexTimezone(context.Background(), cfg, nil, b)
	if gjson.GetBytes(out, "input.0.content").String() != old || gjson.GetBytes(out, "input.2.content").String() != old || gjson.GetBytes(out, "input.3.output").String() != old || gjson.GetBytes(out, "input.4.content").String() != "Please explain: "+old {
		t.Fatal("changed historical context or user/tool content")
	}
	got := gjson.GetBytes(out, "input.1.content.0.text").String()
	if !strings.Contains(got, "<current_date>2025-12-31</current_date>") || !strings.Contains(got, "<timezone>America/Los_Angeles</timezone>") || strings.Contains(got, "unavailable") {
		t.Fatal(got)
	}
	for _, text := range []string{"quoted: " + old, "```xml\n" + old + "\n```"} {
		b := timezoneBody(t, text)
		if string(ApplyCodexTimezone(context.Background(), cfg, nil, b)) != string(b) {
			t.Fatal("changed quoted text")
		}
	}
	for _, b := range [][]byte{[]byte(`{broken`), []byte(`{"input":[{"role":"assistant","content":"<environment_context><timezone>Etc/UTC</timezone></environment_context>"}]}`)} {
		if string(ApplyCodexTimezone(context.Background(), cfg, nil, b)) != string(b) {
			t.Fatal("changed invalid or unrelated body")
		}
	}
}

func TestCodexTimezoneGeneratesMissingFields(t *testing.T) {
	timezoneTestClock(t, "2026-01-01T07:30:00Z")
	t.Setenv("TZ", "America/Los_Angeles")
	cfg := &config.Config{Codex: config.CodexConfig{Timezone: config.CodexTimezoneConfig{Mode: "local"}}}
	for _, text := range []string{"<environment_context><cwd>/work</cwd></environment_context>", "<environment_context><timezone>Etc/UTC</timezone></environment_context>", "<environment_context><current_date>2001-01-01</current_date></environment_context>"} {
		out := ApplyCodexTimezone(context.Background(), cfg, nil, timezoneBody(t, text))
		got := gjson.GetBytes(out, "input.0.content.0.text").String()
		if strings.Count(got, "<current_date>") != 1 || strings.Count(got, "<timezone>") != 1 || !strings.Contains(got, "<current_date>2025-12-31</current_date>") || !strings.Contains(got, "<timezone>America/Los_Angeles</timezone>") {
			t.Fatal(got)
		}
	}
}

func TestCodexTimezoneLocalEnvironmentFallback(t *testing.T) {
	timezoneTestClock(t, "2026-01-01T07:30:00Z")
	for _, value := range []string{"Etc/UTC", "", ":America/Los_Angeles", "invalid/zone"} {
		t.Setenv("TZ", value)
		want := "Etc/UTC"
		if strings.Contains(value, "Los_Angeles") {
			want = "America/Los_Angeles"
		}
		if got := localEnvironmentTimezone(); got != want {
			t.Fatalf("TZ=%q got %q want %q", value, got, want)
		}
		cfg := &config.Config{Codex: config.CodexConfig{Timezone: config.CodexTimezoneConfig{Mode: "local"}}}
		out := ApplyCodexTimezone(context.Background(), cfg, nil, timezoneBody(t, "<environment_context><timezone>Asia/Shanghai</timezone></environment_context>"))
		date := "2026-01-01"
		if want == "America/Los_Angeles" {
			date = "2025-12-31"
		}
		if !strings.Contains(gjson.GetBytes(out, "input.0.content.0.text").String(), "<current_date>"+date+"</current_date>") {
			t.Fatal("fallback timezone and date disagree")
		}
	}
	target := filepath.Join(t.TempDir(), "zoneinfo", "America", "Los_Angeles")
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, nil, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "localtime")
	if err := os.Symlink(target, link); err != nil {
		t.Skip(err)
	}
	if got := environmentTimezoneName(link); got != "America/Los_Angeles" {
		t.Fatal(got)
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
	timezoneTestClock(t, "2026-01-01T07:30:00Z")
	t.Setenv("TZ", "America/Los_Angeles")
	previous := codexTimezoneCache
	t.Cleanup(func() { codexTimezoneCache = previous })
	calls := 0
	codexTimezoneCache = &timezoneResolver{entries: make(map[[32]byte]timezoneEntry), now: time.Now, lookup: func(context.Context, *config.Config, *cliproxyauth.Auth) string { calls++; return "Asia/Tokyo" }}
	cfg := &config.Config{Codex: config.CodexConfig{Timezone: config.CodexTimezoneConfig{Mode: "local"}}}
	body := timezoneBody(t, "<environment_context><timezone>Asia/Shanghai</timezone></environment_context>")
	check := func(mode, want string) {
		t.Helper()
		auth := &cliproxyauth.Auth{ID: "account", Metadata: map[string]any{"codex_timezone_mode": mode}}
		out := ApplyCodexTimezone(context.Background(), cfg, auth, body)
		if !strings.Contains(gjson.GetBytes(out, "input.0.content.0.text").String(), "<timezone>"+want+"</timezone>") {
			t.Fatal(string(out))
		}
		if want == "Asia/Shanghai" && string(out) != string(body) {
			t.Fatal("disabled or invalid mode changed bytes")
		}
	}
	check("off", "Asia/Shanghai")
	check("local", "America/Los_Angeles")
	check("egress", "Asia/Tokyo")
	check("inherit", "America/Los_Angeles")
	check("", "America/Los_Angeles")
	check("fixed", "Asia/Shanghai")
	if calls != 1 {
		t.Fatal("unnecessary GeoIP lookups")
	}
	cfg.Codex.Timezone.Mode = "off"
	check("local", "America/Los_Angeles")
	check("inherit", "Asia/Shanghai")
}

func TestCodexTimezoneEgressDateBoundariesAndDST(t *testing.T) {
	previous := codexTimezoneCache
	t.Cleanup(func() { codexTimezoneCache = previous })
	for _, tc := range []struct{ instant, zone, date string }{
		{"2026-01-01T07:30:00Z", "America/Los_Angeles", "2025-12-31"},
		{"2026-07-01T07:30:00Z", "America/Los_Angeles", "2026-07-01"},
		{"2026-01-01T07:30:00Z", "Asia/Tokyo", "2026-01-01"},
		{"2026-01-01T12:30:00Z", "Pacific/Kiritimati", "2026-01-02"},
	} {
		t.Run(tc.zone+"/"+tc.instant, func(t *testing.T) {
			timezoneTestClock(t, tc.instant)
			codexTimezoneCache = &timezoneResolver{entries: make(map[[32]byte]timezoneEntry), now: time.Now, lookup: func(context.Context, *config.Config, *cliproxyauth.Auth) string { return tc.zone }}
			cfg := &config.Config{Codex: config.CodexConfig{Timezone: config.CodexTimezoneConfig{Mode: "egress"}}}
			out := ApplyCodexTimezone(context.Background(), cfg, nil, timezoneBody(t, "<environment_context><current_date>2001-01-01</current_date><timezone>Etc/UTC</timezone></environment_context>"))
			text := gjson.GetBytes(out, "input.0.content.0.text").String()
			if !strings.Contains(text, "<current_date>"+tc.date+"</current_date>") || !strings.Contains(text, "<timezone>"+tc.zone+"</timezone>") {
				t.Fatal(text)
			}
		})
	}
}
