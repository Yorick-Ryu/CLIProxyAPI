package executor

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCodexTimezoneHTTPAndWebsocketPreparation(t *testing.T) {
	cfg := &config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}, Codex: config.CodexConfig{RequestLocation: config.CodexRequestLocationConfig{CodexRequestLocation: config.CodexRequestLocation{Country: "US", Region: "California", City: "Los Angeles", Timezone: "America/Los_Angeles"}}}}
	body := []byte(`{"model":"gpt-5.6-terra","input":[{"role":"user","content":[{"type":"input_text","text":"\u003cenvironment_context\u003e\n\u003ccurrent_date\u003e2026-09-26\u003c/current_date\u003e\n\u003ctimezone\u003eAsia/Shanghai\u003c/timezone\u003e\n\u003c/environment_context\u003e"}]}]}`)
	req := cliproxyexecutor.Request{Model: "gpt-5.6-terra", Payload: body}
	auth := &cliproxyauth.Auth{ID: "timezone-test", Provider: "codex", Metadata: map[string]any{"codex_request_location_mode": "on"}}
	ctx := context.Background()
	httpReq, upstream, _, err := NewCodexExecutor(cfg).cacheHelper(ctx, sdktranslator.FromString("codex"), "https://example.com/responses", auth, req, body, body)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := io.ReadAll(httpReq.Body)
	if err != nil {
		t.Fatal(err)
	}
	if httpReq.ContentLength != int64(len(wire)) {
		t.Fatal("stale content length")
	}
	if string(wire) != string(upstream) {
		t.Fatal("wire mismatch")
	}
	prepared, err := NewCodexWebsocketsExecutor(cfg).prepareCodexWebsocketStream(ctx, auth, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("codex")})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range [][]byte{wire, prepared.upstreamBody} {
		text := gjson.GetBytes(b, "input.0.content.0.text").String()
		if !strings.Contains(text, "<timezone>America/Los_Angeles</timezone>") || strings.Contains(text, "Asia/Shanghai") || !strings.Contains(text, "<current_date>2026-09-26</current_date>") {
			t.Fatal(text)
		}
	}
	if !strings.Contains(string(body), "Asia/Shanghai") {
		t.Fatal("mutated original request")
	}
}

func TestCodexSearchLocationHTTPAndWebsocketPreparation(t *testing.T) {
	calls := 0
	rt := codexAlphaSearchRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Host != "ipwho.is" {
			t.Fatalf("unexpected request: %s", r.URL.Host)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"success":true,"country_code":"JP","region":"Tokyo","city":"Tokyo","timezone":{"id":"Asia/Tokyo"}}`))}, nil
	})
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(rt))
	cfg := &config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}}
	auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "codex", Metadata: map[string]any{"codex_request_location_mode": "egress"}}
	body := []byte(`{"model":"gpt-5.6-terra","tools":[{"type":"web_search","user_location":{"country":"US"}}],"input":[{"role":"user","content":[{"type":"input_text","text":"<environment_context><timezone>UTC</timezone><current_date>2001-01-01</current_date></environment_context>"}]}]}`)
	req := cliproxyexecutor.Request{Model: "gpt-5.6-terra", Payload: body}
	httpReq, _, _, err := NewCodexExecutor(cfg).cacheHelper(ctx, sdktranslator.FormatCodex, "https://example.com/responses", auth, req, body, body)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := io.ReadAll(httpReq.Body)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := NewCodexWebsocketsExecutor(cfg).prepareCodexWebsocketStream(ctx, auth, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatCodex})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range [][]byte{wire, prepared.upstreamBody} {
		for field, want := range map[string]string{"country": "JP", "region": "Tokyo", "city": "Tokyo", "timezone": "Asia/Tokyo", "type": "approximate"} {
			if got := gjson.GetBytes(b, "tools.0.user_location."+field).String(); got != want {
				t.Fatalf("%s got %q want %q", field, got, want)
			}
		}
		if got := gjson.GetBytes(b, "input.0.content.0.text").String(); !strings.Contains(got, "<timezone>Asia/Tokyo</timezone>") || !strings.Contains(got, "2001-01-01") {
			t.Fatal(got)
		}
	}
	if calls != 1 {
		t.Fatalf("wanted shared lookup, got %d", calls)
	}
}
