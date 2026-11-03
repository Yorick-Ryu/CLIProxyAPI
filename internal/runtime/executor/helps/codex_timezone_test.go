package helps

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func globalLocationConfig() *config.Config {
	return &config.Config{Codex: config.CodexConfig{RequestLocation: config.CodexRequestLocationConfig{
		Enabled:              true,
		CodexRequestLocation: config.CodexRequestLocation{Country: "US", Region: "California", City: "Los Angeles", Timezone: "America/Los_Angeles"},
	}}}
}

func timezoneBody(t *testing.T, text string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"unknown": map[string]any{"keep": true}, "input": []any{map[string]any{"type": "message", "id": "keep-id", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": text}}}}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCodexTimezonePreservesClientDate(t *testing.T) {

	cfg := globalLocationConfig()
	text := "<environment_context>\n<current_date>2026-01-01</current_date>\n<timezone>Asia/Shanghai</timezone>\n<cwd>/work</cwd>\n</environment_context>"
	body := timezoneBody(t, text)
	out := ApplyCodexRequestLocation(context.Background(), cfg, nil, body)
	got := ojson.GetBytes(out, "input.0.content.0.text").String()
	want := strings.ReplaceAll(text, "Asia/Shanghai", "America/Los_Angeles")
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if !ojson.GetBytes(out, "unknown.keep").Bool() || ojson.GetBytes(out, "input.0.id").String() != "keep-id" {
		t.Fatal("lost unrelated fields")
	}
	if string(body) == string(out) {
		t.Fatal("environment not updated")
	}
	again := ApplyCodexRequestLocation(context.Background(), cfg, nil, out)
	if string(again) != string(out) {
		t.Fatal("timezone rewrite is not idempotent")
	}
	cfg.Codex.RequestLocation.Enabled = false
	if string(ApplyCodexRequestLocation(context.Background(), cfg, nil, body)) != string(body) {
		t.Fatal("disabled global location changed bytes")
	}
	if string(ApplyCodexRequestLocation(context.Background(), nil, nil, body)) != string(body) {
		t.Fatal("nil configuration changed bytes")
	}
}

func TestCodexTimezonePreservesStringAndToolContent(t *testing.T) {

	cfg := globalLocationConfig()
	old := "<environment_context><current_date>2001-01-01</current_date><timezone>Asia/Shanghai</timezone></environment_context>"
	latest := "<environment_context>\n  <current_date status=\"unavailable\" />\n  <timezone>Etc/UTC</timezone>\n</environment_context>"
	input := []any{map[string]any{"role": "user", "content": old}, map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": latest}}}, map[string]any{"role": "assistant", "content": old}, map[string]any{"type": "function_call_output", "output": old}, map[string]any{"role": "user", "content": "Please explain: " + old}}
	b, err := json.Marshal(map[string]any{"input": input})
	if err != nil {
		t.Fatal(err)
	}
	out := ApplyCodexRequestLocation(context.Background(), cfg, nil, b)
	if ojson.GetBytes(out, "input.0.content").String() != old || ojson.GetBytes(out, "input.2.content").String() != old || ojson.GetBytes(out, "input.3.output").String() != old || ojson.GetBytes(out, "input.4.content").String() != "Please explain: "+old {
		t.Fatal("changed historical context or user/tool content")
	}
	got := ojson.GetBytes(out, "input.1.content.0.text").String()
	if !strings.Contains(got, "<current_date status=\"unavailable\" />") || !strings.Contains(got, "<timezone>America/Los_Angeles</timezone>") {
		t.Fatal(got)
	}
	for _, text := range []string{"quoted: " + old, "```xml\n" + old + "\n```"} {
		b := timezoneBody(t, text)
		if string(ApplyCodexRequestLocation(context.Background(), cfg, nil, b)) != string(b) {
			t.Fatal("changed quoted text")
		}
	}
	for _, b := range [][]byte{[]byte(`{broken`), []byte(`{"input":[{"role":"assistant","content":"<environment_context><timezone>Etc/UTC</timezone></environment_context>"}]}`)} {
		if string(ApplyCodexRequestLocation(context.Background(), cfg, nil, b)) != string(b) {
			t.Fatal("changed invalid or unrelated body")
		}
	}
}

func TestCodexTimezonePreservesMissingFields(t *testing.T) {

	cfg := globalLocationConfig()
	for _, text := range []string{"<environment_context><cwd>/work</cwd></environment_context>", "<environment_context><timezone>Etc/UTC</timezone></environment_context>", "<environment_context><current_date>2001-01-01</current_date></environment_context>"} {
		out := ApplyCodexRequestLocation(context.Background(), cfg, nil, timezoneBody(t, text))
		got := ojson.GetBytes(out, "input.0.content.0.text").String()
		if got != strings.ReplaceAll(text, "<timezone>Etc/UTC</timezone>", "<timezone>America/Los_Angeles</timezone>") {
			t.Fatal(got)
		}
	}
}

func TestCodexTimezoneAlignsDesktopContextAndHonorsContentKind(t *testing.T) {
	cfg := globalLocationConfig()
	body := []byte(`{"input":[{"role":"developer","content":[{"type":"input_text","text":"<codex_apps_client_time_context><current_date>2026-11-03</current_date><timezone>Asia/Shanghai</timezone></codex_apps_client_time_context>"}],"internal_chat_message_metadata_passthrough":{"content_item_kinds":["additional_content.codex_apps_client_time_context"]}},{"role":"developer","content":[{"type":"input_text","text":"<codex_apps_client_time_context><timezone>Asia/Shanghai</timezone></codex_apps_client_time_context>"}],"internal_chat_message_metadata_passthrough":{"content_item_kinds":["user.text"]}}]}`)
	out := ApplyCodexRequestLocation(context.Background(), cfg, nil, body)
	if !strings.Contains(string(out), "<timezone>America/Los_Angeles</timezone>") || !strings.Contains(string(out), "<timezone>Asia/Shanghai</timezone>") || !strings.Contains(string(out), "<current_date>2026-11-03</current_date>") {
		t.Fatal(string(out))
	}
}

func TestCodexTimezoneCacheExpiryAndIsolation(t *testing.T) {
	now := time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)
	calls := 0
	r := &timezoneResolver{entries: make(map[[32]byte]timezoneEntry), now: func() time.Time { return now }, lookup: func(context.Context, *config.Config, *cliproxyauth.Auth) codexRequestLocation {
		calls++
		return codexRequestLocation{Timezone: "Asia/Tokyo"}
	}}
	cfg := &config.Config{}
	a := &cliproxyauth.Auth{ID: "a", ProxyURL: "socks5://proxy:1080"}
	ctx := context.Background()
	if r.resolve(ctx, cfg, a).Timezone != "Asia/Tokyo" || r.resolve(ctx, cfg, a).Timezone != "Asia/Tokyo" || calls != 1 {
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
	r.lookup = func(context.Context, *config.Config, *cliproxyauth.Auth) codexRequestLocation {
		calls++
		return codexRequestLocation{}
	}
	now = now.Add(7 * time.Hour)
	if r.resolve(ctx, cfg, a).Timezone != "" || r.resolve(ctx, cfg, a).Timezone != "" || calls != 4 {
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
	if lookupEgressTimezone(ctx, &config.Config{}, nil).Timezone != "Asia/Tokyo" || calls != 1 {
		t.Fatal("context transport ignored")
	}
	if lookupEgressTimezone(ctx, &config.Config{}, &cliproxyauth.Auth{ProxyURL: "invalid://secret"}).Timezone != "" || calls != 1 {
		t.Fatal("invalid proxy fell back to direct")
	}
}

func TestCodexTimezoneEgressFailurePreservesBody(t *testing.T) {
	previous := codexTimezoneCache
	t.Cleanup(func() { codexTimezoneCache = previous })
	calls := 0
	codexTimezoneCache = &timezoneResolver{entries: make(map[[32]byte]timezoneEntry), now: time.Now, lookup: func(context.Context, *config.Config, *cliproxyauth.Auth) codexRequestLocation {
		calls++
		return codexRequestLocation{}
	}}
	cfg := &config.Config{}
	auth := &cliproxyauth.Auth{ID: "egress-failure", Metadata: map[string]any{"codex_request_location_mode": "egress"}}
	body := timezoneBody(t, "<environment_context><timezone>Asia/Shanghai</timezone></environment_context>")
	if string(ApplyCodexRequestLocation(context.Background(), cfg, auth, body)) != string(body) || calls != 1 {
		t.Fatal("lookup failure changed body")
	}
	auth.Metadata["codex_request_location_mode"] = "off"
	ApplyCodexRequestLocation(context.Background(), cfg, auth, body)
	if calls != 1 {
		t.Fatal("disabled mode did lookup")
	}
}

func TestCodexTimezoneCanceledLookupDoesNotBlockCaller(t *testing.T) {
	started := make(chan struct{})
	finished := make(chan struct{})
	r := &timezoneResolver{entries: make(map[[32]byte]timezoneEntry), now: time.Now, lookup: func(ctx context.Context, _ *config.Config, _ *cliproxyauth.Auth) codexRequestLocation {
		close(started)
		<-ctx.Done()
		close(finished)
		return codexRequestLocation{}
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan codexRequestLocation, 1)
	go func() { result <- r.resolve(ctx, &config.Config{}, nil) }()
	<-started
	cancel()
	if (<-result).Timezone != "" {
		t.Fatal("canceled lookup returned zone")
	}
	<-finished
}

func TestCodexTimezoneCanceledLeaderRetriesForWaiters(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		release := make(chan struct{})
		r := &timezoneResolver{entries: make(map[[32]byte]timezoneEntry), now: time.Now, lookup: func(ctx context.Context, _ *config.Config, _ *cliproxyauth.Auth) codexRequestLocation {
			calls++
			if calls == 1 {
				<-ctx.Done()
				return codexRequestLocation{}
			}
			<-release
			return codexRequestLocation{Timezone: "Asia/Tokyo"}
		}}
		cfg := &config.Config{}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		leader := make(chan codexRequestLocation, 1)
		go func() { leader <- r.resolve(ctx, cfg, nil) }()
		synctest.Wait()
		waiters := make(chan codexRequestLocation, 2)
		for range 2 {
			go func() { waiters <- r.resolve(context.Background(), cfg, nil) }()
		}
		synctest.Wait()
		cancel()
		synctest.Wait()
		close(release)
		if got := <-leader; got.Timezone != "" {
			t.Fatalf("canceled leader returned %v", got)
		}
		for range 2 {
			if got := <-waiters; got.Timezone != "Asia/Tokyo" {
				t.Errorf("live waiter lost location after leader cancellation: %v", got)
			}
		}
		if got := r.resolve(context.Background(), cfg, nil); got.Timezone != "Asia/Tokyo" {
			t.Errorf("cancellation poisoned cache: %v", got)
		}
		if calls != 2 {
			t.Errorf("lookups = %d, want one canceled lookup and one shared retry", calls)
		}
	})
}

func TestCodexTimezoneCanceledWaiterLeavesLookupRunning(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		release := make(chan struct{})
		r := &timezoneResolver{entries: make(map[[32]byte]timezoneEntry), now: time.Now, lookup: func(context.Context, *config.Config, *cliproxyauth.Auth) codexRequestLocation {
			calls++
			<-release
			return codexRequestLocation{Timezone: "Asia/Tokyo"}
		}}
		cfg := &config.Config{}
		leader := make(chan codexRequestLocation, 1)
		go func() { leader <- r.resolve(context.Background(), cfg, nil) }()
		synctest.Wait()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		waiter := make(chan codexRequestLocation, 1)
		go func() { waiter <- r.resolve(ctx, cfg, nil) }()
		synctest.Wait()
		cancel()
		if got := <-waiter; got.Timezone != "" {
			t.Fatalf("canceled waiter returned %v", got)
		}
		close(release)
		if got := <-leader; got.Timezone != "Asia/Tokyo" {
			t.Fatalf("waiter cancellation affected leader: %v", got)
		}
		if got := r.resolve(context.Background(), cfg, nil); got.Timezone != "Asia/Tokyo" || calls != 1 {
			t.Fatalf("successful shared lookup not cached: location=%v calls=%d", got, calls)
		}
	})
}

func TestCodexEgressSearchLocation(t *testing.T) {
	previous := codexTimezoneCache
	t.Cleanup(func() { codexTimezoneCache = previous })
	codexTimezoneCache = &timezoneResolver{entries: make(map[[32]byte]timezoneEntry), now: time.Now, lookup: lookupEgressTimezone}
	calls := 0
	rt := timezoneRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Query().Get("fields") != "success,country_code,region,city,timezone.id" || r.Header.Get("Authorization") != "" {
			t.Fatal("incorrect GeoIP request")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"success":true,"country_code":"JP","region":"Tokyo","city":"Tokyo","timezone":{"id":"Asia/Tokyo"}}`))}, nil
	})
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(rt))
	cfg := &config.Config{}
	auth := &cliproxyauth.Auth{ID: "search-location", Metadata: map[string]any{"codex_request_location_mode": "egress"}}
	body := []byte(`{"tools":[{"type":"web_search","external_web_access":false,"user_location":{"country":"US","extra":"remove"}},{"type":"web_search_preview"},{"type":"web_search_future"},{"type":"function","name":"web_search"},{"type":"namespace","tools":[{"type":"web_search"}]}]}`)
	want := `{"type":"approximate","country":"JP","region":"Tokyo","city":"Tokyo","timezone":"Asia/Tokyo"}`
	for attempt := 0; attempt < 2; attempt++ {
		out := ApplyCodexRequestLocation(ctx, cfg, auth, body)
		for _, path := range []string{"tools.0.user_location", "tools.1.user_location", "tools.2.user_location"} {
			got := ojson.GetBytes(out, path)
			for _, field := range []string{"type", "country", "region", "city", "timezone"} {
				if got.Get(field).String() != ojson.Get(want, field).String() {
					t.Fatalf("%s: %s", path, out)
				}
			}
			if len(got.Map()) != 5 {
				t.Fatal("did not replace entire location")
			}
		}
		if ojson.GetBytes(out, "tools.0.external_web_access").Bool() || ojson.GetBytes(out, "tools.3.user_location").Exists() || ojson.GetBytes(out, "tools.4.tools.0.user_location").Exists() {
			t.Fatal("changed unrelated tool fields")
		}
	}
	if calls != 1 {
		t.Fatal("did not share location cache")
	}
	if !strings.Contains(string(body), `"extra":"remove"`) {
		t.Fatal("mutated input")
	}
	auth.Metadata["codex_request_location_mode"] = "off"
	if string(ApplyCodexRequestLocation(ctx, cfg, auth, body)) != string(body) {
		t.Fatal("off changed search location")
	}
	if calls != 1 {
		t.Fatal("off issued a GeoIP query")
	}
	for _, location := range []codexRequestLocation{{}, {Timezone: "Asia/Tokyo"}, {Country: "JP", Region: "Tokyo", City: "Tokyo", Timezone: "invalid"}} {
		if string(alignWebSearchLocation(body, location)) != string(body) {
			t.Fatal("incomplete location changed body")
		}
	}
}

func TestCodexTimeContextXMLBoundaries(t *testing.T) {
	for _, text := range []string{
		`<environment_context><example><timezone>UTC</timezone></example></environment_context>`,
		`<environment_context><timezone>UTC</timezone><broken></environment_context>`,
		`<environment_context><timezone>UTC</timezone></environment_context><environment_context></environment_context>`,
		`<environment_context><!-- <timezone>UTC</timezone> --></environment_context>`,
		`<environment_context><![CDATA[<timezone>UTC</timezone>]]></environment_context>`,
	} {
		if got := alignTimeContextText(text, "environment_context", "Asia/Tokyo"); got != text {
			t.Fatalf("modified invalid/nested text: %s", got)
		}
	}
	text := ` <environment_context><timezone/><example><timezone>UTC</timezone></example><timezone>UTC</timezone><current_date>2001-01-01</current_date></environment_context> `
	want := ` <environment_context><timezone>Asia/Tokyo</timezone><example><timezone>UTC</timezone></example><timezone>Asia/Tokyo</timezone><current_date>2001-01-01</current_date></environment_context> `
	if got := alignTimeContextText(text, "environment_context", "Asia/Tokyo"); got != want {
		t.Fatalf("got %s", got)
	}
	body := []byte(`{"input":[{"role":"user","internal_chat_message_metadata_passthrough":{"content_item_kinds":[]},"content":[{"type":"input_text","text":"<environment_context><timezone>UTC</timezone></environment_context>"}]}]}`)
	if string(alignEnvironmentTime(body, "Asia/Tokyo")) != string(body) {
		t.Fatal("empty explicit classification must suppress rewrite")
	}
}

func TestCodexRequestLocationPrecedence(t *testing.T) {
	previous := codexTimezoneCache
	t.Cleanup(func() { codexTimezoneCache = previous })
	calls := 0
	codexTimezoneCache = &timezoneResolver{entries: make(map[[32]byte]timezoneEntry), now: time.Now, lookup: func(context.Context, *config.Config, *cliproxyauth.Auth) codexRequestLocation {
		calls++
		return codexRequestLocation{Country: "JP", Region: "Tokyo", City: "Tokyo", Timezone: "Asia/Tokyo"}
	}}
	accountLocation := map[string]any{"country": "DE", "region": "Berlin", "city": "Berlin", "timezone": "Europe/Berlin"}
	body := []byte(`{"tools":[{"type":"web_search"}],"input":[{"role":"user","content":[{"type":"input_text","text":"<environment_context><timezone>Asia/Shanghai</timezone></environment_context>"}]}]}`)
	cfg := globalLocationConfig()
	for _, tc := range []struct {
		name     string
		global   bool
		metadata map[string]any
		want     string
	}{
		{"inherit enabled global", true, map[string]any{}, "America/Los_Angeles"},
		{"inherit disabled global", false, map[string]any{}, "Asia/Shanghai"},
		{"inherit ignores account location", false, map[string]any{"codex_request_location": accountLocation}, "Asia/Shanghai"},
		{"on uses account location", false, map[string]any{"codex_request_location_mode": "on", "codex_request_location": accountLocation}, "Europe/Berlin"},
		{"on falls back to global", false, map[string]any{"codex_request_location_mode": "on"}, "America/Los_Angeles"},
		{"off blocks account location", true, map[string]any{"codex_request_location_mode": "off", "codex_request_location": accountLocation}, "Asia/Shanghai"},
		{"egress", false, map[string]any{"codex_request_location_mode": "egress", "codex_request_location": accountLocation}, "Asia/Tokyo"},
		{"invalid mode", true, map[string]any{"codex_request_location_mode": "local"}, "Asia/Shanghai"},
		{"invalid location", true, map[string]any{"codex_request_location_mode": "on", "codex_request_location": map[string]any{"timezone": "Europe/Berlin"}}, "Asia/Shanghai"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg.Codex.RequestLocation.Enabled = tc.global
			out := ApplyCodexRequestLocation(context.Background(), cfg, &cliproxyauth.Auth{ID: tc.name, Metadata: tc.metadata}, body)
			if !strings.Contains(ojson.GetBytes(out, "input.0.content.0.text").String(), "<timezone>"+tc.want+"</timezone>") {
				t.Fatal(string(out))
			}
			if tc.want == "Asia/Shanghai" && ojson.GetBytes(out, "tools.0.user_location").Exists() {
				t.Fatal("disabled location changed web search")
			}
			if tc.want != "Asia/Shanghai" && ojson.GetBytes(out, "tools.0.user_location.timezone").String() != tc.want {
				t.Fatal(string(out))
			}
		})
	}
	if calls != 1 {
		t.Fatalf("egress lookups = %d", calls)
	}
}
