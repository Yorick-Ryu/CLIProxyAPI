package executor

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCodexSIWCPayloadRulesAfterCleanupAndIdentity(t *testing.T) {
	for _, mode := range []string{"http", "http-stream", "ws", "ws-stream", "compact"} {
		t.Run(mode, func(t *testing.T) {
			captured := make(chan []byte, 1)
			completed := []byte(`{"type":"response.completed","response":{"id":"resp_siwc_barrier","status":"completed","output":[],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "ws" || mode == "ws-stream" {
					conn, errUpgrade := (&websocket.Upgrader{}).Upgrade(w, r, nil)
					if errUpgrade != nil {
						t.Error(errUpgrade)
						return
					}
					defer func() { _ = conn.Close() }()
					_, body, errRead := conn.ReadMessage()
					if errRead != nil {
						t.Error(errRead)
						return
					}
					captured <- body
					if errWrite := conn.WriteMessage(websocket.TextMessage, completed); errWrite != nil {
						t.Error(errWrite)
					}
					return
				}
				body, errRead := io.ReadAll(r.Body)
				if errRead != nil {
					t.Error(errRead)
					return
				}
				captured <- body
				if mode == "compact" {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"id":"resp_compact","object":"response.compaction","output":[],"usage":{"input_tokens":0,"output_tokens":0}}`))
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write(append(append([]byte("data: "), completed...), '\n', '\n'))
			}))
			defer server.Close()

			models := []config.PayloadModelRule{{Name: "*"}}
			cfg := &config.Config{
				Codex: config.CodexConfig{IdentityConvergence: true},
				Payload: config.PayloadConfig{
					Override: []config.PayloadRule{{
						Models: models,
						Params: map[string]any{"temperature": 0.25, "store": true, "prompt_cache_key": "configured-cache"},
					}, {
						Models: []config.PayloadModelRule{{Name: "*", NotExist: []string{"max_output_tokens"}}},
						Params: map[string]any{"cleanup_observed": true},
					}},
					Filter: []config.PayloadFilterRule{{Models: models, Params: []string{"input.0", "instructions", "client_metadata"}}},
				},
			}
			auth := &cliproxyauth.Auth{
				ID: t.Name(), Provider: "codex", Attributes: map[string]string{"base_url": server.URL},
				Metadata: map[string]any{"use_siwc": true, "access_token": "synthetic-outer", "account_id": "synthetic-account", "siwc": map[string]any{"access_token": "synthetic-siwc"}},
			}
			var executor cliproxyauth.ProviderExecutor = NewCodexExecutor(cfg)
			if mode == "ws" || mode == "ws-stream" {
				executor = NewCodexWebsocketsExecutor(cfg)
			}
			payload := []byte(`{"model":"gpt-5.4","instructions":"explicit","input":[{"role":"user","content":"remove"},{"role":"user","content":"keep"}],"temperature":0.7,"max_output_tokens":123,"prompt_cache_key":"original-cache","client_metadata":{"session_id":"original-session","thread_id":"original-thread"}}`)
			req := cliproxyexecutor.Request{Model: "gpt-5.4", Payload: payload}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}
			if mode == "compact" {
				opts.Alt = "responses/compact"
			}
			ctx := codexAccountIdentityTestContext("synthetic-caller")
			if mode == "http-stream" || mode == "ws-stream" {
				result, errStream := executor.ExecuteStream(ctx, auth, req, opts)
				if errStream != nil {
					t.Fatal(errStream)
				}
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
				}
			} else if _, errExecute := executor.Execute(ctx, auth, req, opts); errExecute != nil {
				t.Fatal(errExecute)
			}

			body := <-captured
			if gjson.GetBytes(body, "temperature").Float() != 0.25 || !gjson.GetBytes(body, "store").Bool() || gjson.GetBytes(body, "prompt_cache_key").String() != "configured-cache" {
				t.Fatalf("cleanup or identity overwrote payload rules: %s", body)
			}
			if !gjson.GetBytes(body, "cleanup_observed").Bool() {
				t.Fatalf("rules ran before SIWC cleanup: %s", body)
			}
			if gjson.GetBytes(body, "instructions").Exists() || gjson.GetBytes(body, "client_metadata").Exists() {
				t.Fatalf("filtered identity or instructions restored: %s", body)
			}
			input := gjson.GetBytes(body, "input").Array()
			if len(input) != 1 || input[0].Get("content").String() != "keep" {
				t.Fatalf("payload filter must run exactly once: %s", body)
			}
		})
	}
}
