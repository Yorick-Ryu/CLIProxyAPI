package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

// Identical fixtures and benchmark entry points are retained on both maintenance
// branches, so a branch comparison never measures a different benchmark body.
func runtimeReviewFixture(turns, textSize int, identities bool) []byte {
	const turn = "0199c123-4567-7890-8123-456789abcdef"
	input := make([]any, 0, turns)
	for i := 0; i < turns; i++ {
		item := map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": strings.Repeat("x", textSize)}}}
		if identities {
			item["internal_chat_message_metadata_passthrough"] = map[string]any{"turn_id": fmt.Sprintf("history-%d", i)}
		}
		input = append(input, item)
	}
	raw := map[string]any{"model": "gpt-6-astra", "input": input, "instructions": "synthetic benchmark"}
	if identities {
		raw["prompt_cache_key"] = "session"
		raw["client_metadata"] = map[string]any{"session_id": "session", "thread_id": "thread", "turn_id": turn, "window_number": 3, "x-codex-window-id": "thread:3", "x-codex-turn-metadata": `{"turn_id":"` + turn + `","session_id":"session","thread_id":"thread","window_number":3,"analytics_enabled":true,"unknown":{"z":1e+09,"a":"中文"}}`}
	}
	body, err := json.Marshal(raw)
	if err != nil {
		panic(err)
	}
	return body
}

func runtimeReviewPipeline(body []byte, credential *cliproxyauth.Auth, cfg *config.Config, headers http.Header) ([]byte, http.Header) {
	body, identity := applyCodexAccountIdentityBody(context.Background(), credential, body)
	body, convergence := applyCodexIdentityConvergenceBody(cfg, credential, body, body, headers)
	body = helps.CanonicalCodexTurnMetadataBody(body)
	headers = headers.Clone()
	applyCodexAccountIdentityHeaders(headers, &identity)
	applyCodexIdentityConvergenceHeaders(headers, &convergence)
	helps.CanonicalCodexTurnMetadataHeaders(headers)
	return body, headers
}

func BenchmarkCodexRuntimeReview(b *testing.B) {
	for _, shape := range []struct {
		name        string
		turns, size int
		ids         bool
	}{
		{"minimal", 1, 32, false}, {"small", 4, 128, true}, {"history128", 128, 1024, true}, {"large2MiB", 64, 32768, true},
	} {
		body := runtimeReviewFixture(shape.turns, shape.size, shape.ids)
		for _, mode := range []string{"off", "device", "full"} {
			credential := &cliproxyauth.Auth{ID: "review-account", Provider: "codex", Metadata: map[string]any{"account_id": "review-account", codexIdentityConvergenceModeKey: mode}}
			cfg := &config.Config{}
			headers := http.Header{"Session-Id": {"session"}, "Thread-Id": {"thread"}, "X-Codex-Turn-Metadata": {`{"turn_id":"0199c123-4567-7890-8123-456789abcdef","analytics_enabled":true}`}}
			b.Run(shape.name+"/"+mode, func(b *testing.B) {
				b.ReportAllocs()
				b.SetBytes(int64(len(body)))
				for b.Loop() {
					runtimeReviewPipeline(body, credential, cfg, headers)
				}
			})
		}
	}
}

func BenchmarkCodexRuntimeReviewHTTP(b *testing.B) {
	for _, shape := range []struct {
		name        string
		turns, size int
		ids         bool
	}{
		{"minimal", 1, 32, false}, {"small", 4, 128, true}, {"history128", 128, 1024, true}, {"large2MiB", 64, 32768, true},
	} {
		for _, compressed := range []bool{false, true} {
			name := fmt.Sprintf("%s/zstd%t", shape.name, compressed)
			b.Run(name, func(b *testing.B) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if _, err := io.Copy(io.Discard, r.Body); err != nil {
						b.Error(err)
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"benchmark\",\"status\":\"completed\",\"output\":[]}}\n\n")
				}))
				defer server.Close()
				cfg := &config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}, Codex: config.CodexConfig{IdentityConvergence: true, RequestCompression: config.CodexRequestCompressionConfig{Enabled: compressed}}}
				credential := &cliproxyauth.Auth{ID: "review-http", Provider: "codex", Attributes: map[string]string{"base_url": server.URL}, Metadata: map[string]any{"account_id": "review-http", "access_token": "synthetic-fixture"}}
				body := runtimeReviewFixture(shape.turns, shape.size, shape.ids)
				req := cliproxyexecutor.Request{Model: "gpt-6-astra", Payload: body}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatCodex, Headers: http.Header{"Session-Id": {"session"}, "Thread-Id": {"thread"}}}
				executor := NewCodexExecutor(cfg)
				b.ReportAllocs()
				b.SetBytes(int64(len(body)))
				for b.Loop() {
					if _, err := executor.Execute(context.Background(), credential, req, opts); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

func TestCodexRuntimeReviewIsolationMatrix(t *testing.T) {
	for _, mode := range []string{"off", "device", "session", "full"} {
		t.Run(mode, func(t *testing.T) {
			source := runtimeReviewFixture(8, 32, true)
			sourceCopy := bytes.Clone(source)
			headers := http.Header{"Session-Id": {"session"}, "Thread-Id": {"thread"}, "X-Codex-Turn-Metadata": {`{"turn_id":"0199c123-4567-7890-8123-456789abcdef","session_id":"session","thread_id":"thread"}`}}
			cfg := &config.Config{}
			a := &cliproxyauth.Auth{ID: "file-a", Provider: "codex", Metadata: map[string]any{"account_id": "account-a", codexIdentityConvergenceModeKey: mode}}
			equivalent := &cliproxyauth.Auth{ID: "renamed-file", Provider: "codex", Metadata: map[string]any{"account_id": "account-a", codexIdentityConvergenceModeKey: mode}}
			other := &cliproxyauth.Auth{ID: "file-b", Provider: "codex", Metadata: map[string]any{"account_id": "account-b", codexIdentityConvergenceModeKey: mode}}
			first, h1 := runtimeReviewPipeline(source, a, cfg, headers)
			retry, h2 := runtimeReviewPipeline(source, equivalent, cfg, headers)
			second, h3 := runtimeReviewPipeline(source, other, cfg, headers)
			if !bytes.Equal(first, retry) || h1.Get("Thread-Id") != h2.Get("Thread-Id") {
				t.Fatal("stable account identity changed across file/retry")
			}
			if bytes.Equal(first, second) || h1.Get("Thread-Id") == h3.Get("Thread-Id") {
				t.Fatal("cross-account identity collision")
			}
			for _, path := range []string{"client_metadata.turn_id", "input.0.internal_chat_message_metadata_passthrough.turn_id", "prompt_cache_key"} {
				if gjson.GetBytes(first, path).String() == gjson.GetBytes(second, path).String() {
					t.Fatalf("account isolation lost: %s", path)
				}
			}
			if !bytes.Equal(source, sourceCopy) {
				t.Fatal("source body changed")
			}
			// Independent buffers may be processed concurrently for different accounts.
			done := make(chan []byte, 16)
			for i := 0; i < 16; i++ {
				go func() { body, _ := runtimeReviewPipeline(source, a, cfg, headers); done <- body }()
			}
			for i := 0; i < 16; i++ {
				if !bytes.Equal(<-done, first) {
					t.Fatal("concurrent identity output differs")
				}
			}
		})
	}
}
