package executor

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCodexTimezoneHTTPAndWebsocketPreparation(t *testing.T) {
	cfg := &config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}, Codex: config.CodexConfig{Timezone: config.CodexTimezoneConfig{Mode: "off"}}}
	body := []byte(`{"model":"gpt-5.6-terra","input":[{"role":"user","content":[{"type":"input_text","text":"\u003cenvironment_context\u003e\n\u003ccurrent_date\u003e2026-09-26\u003c/current_date\u003e\n\u003ctimezone\u003eAsia/Shanghai\u003c/timezone\u003e\n\u003c/environment_context\u003e"}]}]}`)
	req := cliproxyexecutor.Request{Model: "gpt-5.6-terra", Payload: body}
	auth := &cliproxyauth.Auth{ID: "timezone-test", Provider: "codex", Metadata: map[string]any{"codex_timezone_mode": "fixed", "codex_timezone": "Pacific/Honolulu"}}
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
		if !strings.Contains(text, "<timezone>Pacific/Honolulu</timezone>") || strings.Contains(text, "Asia/Shanghai") {
			t.Fatal(text)
		}
	}
	if !strings.Contains(string(body), "Asia/Shanghai") {
		t.Fatal("mutated original request")
	}
}
