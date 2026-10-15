package executor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/klauspost/compress/zstd"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCodexAnalyticsOnWire(t *testing.T) {
	for _, transport := range []string{"http", "websocket"} {
		for _, stream := range []bool{false, true} {
			for _, mode := range []string{"default", "default-false", "disabled", "enabled", "payload-override", "payload-filter"} {
				t.Run(transport+map[bool]string{false: "/execute/", true: "/stream/"}[stream]+mode, func(t *testing.T) {
					type wireRequest struct {
						body    []byte
						headers http.Header
					}
					captured := make(chan wireRequest, 1)
					completed := []byte(`{"type":"response.completed","response":{"id":"resp_analytics","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":0,"total_tokens":1}}}`)
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if transport == "websocket" {
							conn, err := (&websocket.Upgrader{EnableCompression: true}).Upgrade(w, r, nil)
							if err != nil {
								t.Error(err)
								return
							}
							defer func() { _ = conn.Close() }()
							_, body, errRead := conn.ReadMessage()
							if errRead != nil {
								t.Error(errRead)
								return
							}
							captured <- wireRequest{body, r.Header.Clone()}
							if errWrite := conn.WriteMessage(websocket.TextMessage, completed); errWrite != nil {
								t.Error(errWrite)
							}
							return
						}
						if r.Header.Get("Content-Encoding") != "zstd" {
							t.Error("expected compressed OAuth request")
							w.WriteHeader(http.StatusBadRequest)
							return
						}
						decoder, err := zstd.NewReader(r.Body)
						if err != nil {
							t.Error(err)
							return
						}
						defer decoder.Close()
						body, errRead := io.ReadAll(decoder)
						if errRead != nil {
							t.Error(errRead)
							return
						}
						captured <- wireRequest{body, r.Header.Clone()}
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = w.Write(append(append([]byte("data: "), completed...), '\n', '\n'))
					}))
					defer server.Close()
					cfg := &config.Config{
						SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll},
						Codex: config.CodexConfig{
							IdentityConvergence: true,
							RequestCompression:  config.CodexRequestCompressionConfig{Enabled: true},
						},
					}
					if !strings.HasPrefix(mode, "default") {
						cfg.Codex.AnalyticsEnabled = new(mode == "enabled")
					}
					metadata := `{"analytics_enabled":true,"session_id":"client-session","thread_id":"client-session","marker":"keep"}`
					if mode == "enabled" || mode == "default-false" {
						metadata = strings.Replace(metadata, `"analytics_enabled":true`, `"analytics_enabled":false`, 1)
					}
					if mode == "payload-override" {
						cfg.Payload.Override = []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "*"}}, Params: map[string]any{"client_metadata.x-codex-turn-metadata": metadata}}}
					} else if mode == "payload-filter" {
						cfg.Payload.Filter = []config.PayloadFilterRule{{Models: []config.PayloadModelRule{{Name: "*"}}, Params: []string{"client_metadata"}}}
					}
					body, err := json.Marshal(map[string]any{
						"model": "gpt-6-luna", "input": []any{map[string]any{"role": "user", "content": "test"}},
						"client_metadata": map[string]any{"session_id": "client-session", "x-codex-installation-id": "device", "x-codex-turn-metadata": metadata},
					})
					if err != nil {
						t.Fatal(err)
					}
					var executor cliproxyauth.ProviderExecutor = NewCodexExecutor(cfg)
					if transport == "websocket" {
						executor = NewCodexWebsocketsExecutor(cfg)
					}
					auth := &cliproxyauth.Auth{ID: "analytics-test", Provider: "codex", Attributes: map[string]string{"base_url": server.URL}, Metadata: map[string]any{"access_token": "test-token", "account_id": "test-account"}}
					req := cliproxyexecutor.Request{Model: "gpt-6-luna", Payload: body}
					opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatCodex, Headers: http.Header{"X-Codex-Turn-Metadata": {metadata}}}
					if stream {
						result, err := executor.ExecuteStream(context.Background(), auth, req, opts)
						if err != nil {
							t.Fatal(err)
						}
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								t.Fatal(chunk.Err)
							}
						}
					} else if _, err := executor.Execute(context.Background(), auth, req, opts); err != nil {
						t.Fatal(err)
					}
					select {
					case wire := <-captured:
						wantHeader := gjson.False
						if mode == "default" || mode == "enabled" {
							wantHeader = gjson.True
						}
						hm := gjson.Parse(wire.headers.Get("X-Codex-Turn-Metadata"))
						if hm.Get("analytics_enabled").Type != wantHeader || hm.Get("marker").String() != "keep" {
							t.Fatalf("unexpected upstream metadata header: %s", hm.Raw)
						}
						if mode == "payload-filter" {
							if gjson.GetBytes(wire.body, "client_metadata").Exists() {
								t.Fatal("restored filtered client metadata")
							}
						} else {
							wantBody := wantHeader
							if mode == "payload-override" {
								wantBody = gjson.True
							}
							bm := gjson.Parse(gjson.GetBytes(wire.body, "client_metadata.x-codex-turn-metadata").String())
							if bm.Get("analytics_enabled").Type != wantBody || bm.Get("marker").String() != "keep" {
								t.Fatalf("unexpected upstream body metadata: %s", bm.Raw)
							}
						}
					default:
						t.Fatal("no upstream request captured")
					}
				})
			}
		}
	}
}

func TestCodexAnalyticsCustomHeaders(t *testing.T) {
	cfg := &config.Config{Codex: config.CodexConfig{AnalyticsEnabled: new(false)}}
	for _, siwc := range []bool{false, true} {
		auth := &cliproxyauth.Auth{Provider: "codex", Attributes: map[string]string{"header:X-Codex-Turn-Metadata": `{"analytics_enabled":true,"marker":"admin"}`}}
		if siwc {
			auth.Metadata = map[string]any{"use_siwc": true}
		}
		r, err := http.NewRequest(http.MethodPost, "https://example.invalid/responses", nil)
		if err != nil {
			t.Fatal(err)
		}
		applyCodexHeadersFromSources(r, auth, "test-token", false, cfg, nil)
		ws := applyCodexWebsocketHeaders(context.Background(), nil, auth, "test-token", cfg, true)
		for _, h := range []http.Header{r.Header, ws} {
			m := gjson.Parse(h.Get("X-Codex-Turn-Metadata"))
			if m.Get("analytics_enabled").Type != gjson.False || m.Get("marker").String() != "admin" {
				t.Fatalf("custom headers bypassed analytics policy: %v", h)
			}
		}
	}
}
