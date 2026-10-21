package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCodexWindowAndMemoryMetadataOnWire(t *testing.T) {
	const thread = "019a0992-1400-7abc-8def-0123456789ab"
	const window = "019a0992-1500-7abc-8def-0123456789ab"
	metadata := `{"thread_id":"` + thread + `","session_id":"` + thread + `","window_id":"` + thread + `:3","window_number":3,"context_window_id":"` + window + `","tool_namespaces_info":{"synthetic":{"name":"synthetic","functions":{}}},"extension":"keep"}`
	for _, transport := range []string{"http", "ws"} {
		for _, stream := range []bool{false, true} {
			for _, mode := range []string{"device", "session", "full"} {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", transport, stream, mode), func(t *testing.T) {
					captured := make(chan *http.Request, 1)
					bodies := make(chan []byte, 1)
					completed := []byte(`{"type":"response.completed","response":{"id":"resp_test","status":"completed","output":[]}}`)
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						captured <- r.Clone(context.Background())
						if transport == "ws" {
							conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
							if err != nil {
								t.Error(err)
								return
							}
							defer conn.Close()
							_, body, err := conn.ReadMessage()
							if err != nil {
								t.Error(err)
								return
							}
							bodies <- body
							_ = conn.WriteMessage(websocket.TextMessage, completed)
						} else {
							body, _ := io.ReadAll(r.Body)
							bodies <- body
							w.Header().Set("Content-Type", "text/event-stream")
							_, _ = fmt.Fprintf(w, "data: %s\n\n", completed)
						}
					}))
					defer server.Close()
					cfg := &config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}}
					auth := &cliproxyauth.Auth{ID: "window-test", Provider: "codex", Attributes: map[string]string{"base_url": server.URL, codexIdentityConvergenceModeKey: mode}, Metadata: map[string]any{"account_id": "synthetic-account"}}
					payload, _ := json.Marshal(map[string]any{"model": "gpt-5.6-sol", "input": []any{}, "client_metadata": map[string]any{"session_id": thread, "thread_id": thread, "context_window_id": window, "window_id": thread + ":3", "x-codex-window-id": thread + ":3", "window_number": 3, "x-codex-turn-metadata": metadata}})
					req := cliproxyexecutor.Request{Model: "gpt-5.6-sol", Payload: payload}
					opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatCodex, Headers: http.Header{"Session-Id": {thread}, "Thread-Id": {thread}, "X-Codex-Window-Id": {thread + ":3"}, "X-Codex-Turn-Metadata": {metadata}, "X-Openai-Memgen-Request": {"true"}}}
					var executor cliproxyauth.ProviderExecutor = NewCodexExecutor(cfg)
					if transport == "ws" {
						executor = NewCodexWebsocketsExecutor(cfg)
					}
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
					h := (<-captured).Header
					body := <-bodies
					bodyMetadata := gjson.Parse(gjson.GetBytes(body, "client_metadata.x-codex-turn-metadata").String())
					headerMetadata := gjson.Parse(h.Get("X-Codex-Turn-Metadata"))
					if h.Get("X-Openai-Memgen-Request") != "true" {
						t.Fatal("memory request marker was lost")
					}
					if !bodyMetadata.Get("tool_namespaces_info").Exists() || headerMetadata.Get("tool_namespaces_info").Exists() {
						t.Fatal("tool inventory projection differs from native Codex")
					}
					if bodyMetadata.Get("extension").String() != "keep" || headerMetadata.Get("extension").String() != "keep" {
						t.Fatal("unknown metadata was lost")
					}
					mappedWindow := bodyMetadata.Get("context_window_id").String()
					assertCodexIdentityV7Timestamp(t, mappedWindow, window)
					if mappedWindow == window || headerMetadata.Get("context_window_id").String() != mappedWindow || gjson.GetBytes(body, "client_metadata.context_window_id").String() != mappedWindow {
						t.Fatal("context window is not consistently isolated")
					}
					windowID := bodyMetadata.Get("window_id").String()
					if !strings.HasSuffix(windowID, ":3") || h.Get("X-Codex-Window-Id") != windowID || gjson.GetBytes(body, "client_metadata.window_id").String() != windowID || gjson.GetBytes(body, "client_metadata.x-codex-window-id").String() != windowID || headerMetadata.Get("window_id").String() != windowID || bodyMetadata.Get("window_number").Int() != 3 || headerMetadata.Get("window_number").Int() != 3 {
						t.Fatal("window ordinal or projections diverged")
					}
				})
			}
		}
	}
}

func TestCodexTurnStateOnWireAndPayloadOverrides(t *testing.T) {
	for _, transport := range []string{"http", "ws"} {
		for _, stream := range []bool{false, true} {
			for _, mode := range []string{"same", "foreign", "unknown", "override", "api-key"} {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", transport, stream, mode), func(t *testing.T) {
					token := t.Name() + "-opaque"
					payload, _ := json.Marshal(map[string]any{"model": "gpt-5.6-sol", "input": []any{}, "client_metadata": map[string]any{"x-codex-turn-state": token}})
					req := cliproxyexecutor.Request{Model: "gpt-5.6-sol", Payload: payload}
					opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatCodex, Headers: http.Header{"Thread-Id": {t.Name()}, "X-Codex-Turn-State": {token}}}
					cfg := &config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}}
					if mode == "override" {
						cfg.Payload.Override = []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "*"}}, Params: map[string]any{"client_metadata.x-codex-turn-state": token}}}
					}
					owner := &cliproxyauth.Auth{ID: "auth-a", Provider: "codex", Metadata: map[string]any{"account_id": "synthetic-a"}}
					ctx := context.Background()
					if mode != "unknown" {
						helps.CodexTurnStateCommitObserver(codexTurnStateContext(ctx, owner, req, opts))(http.Header{"X-Codex-Turn-State": {token}}, nil)
					}
					captured := make(chan [2]string, 1)
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						gotHeader := r.Header.Get("X-Codex-Turn-State")
						completed := []byte(`{"type":"response.completed","response":{"id":"resp_test","status":"completed","output":[]}}`)
						if transport == "ws" {
							conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
							if err != nil {
								t.Error(err)
								return
							}
							defer conn.Close()
							_, body, err := conn.ReadMessage()
							if err != nil {
								t.Error(err)
								return
							}
							gotBody := gjson.GetBytes(body, "client_metadata.x-codex-turn-state").String()
							captured <- [2]string{gotHeader, gotBody}
							_ = conn.WriteMessage(websocket.TextMessage, completed)
						} else {
							body, _ := io.ReadAll(r.Body)
							gotBody := gjson.GetBytes(body, "client_metadata.x-codex-turn-state").String()
							captured <- [2]string{gotHeader, gotBody}
							w.Header().Set("Content-Type", "text/event-stream")
							_, _ = fmt.Fprintf(w, "data: %s\n\n", completed)
						}
					}))
					defer server.Close()
					auth := &cliproxyauth.Auth{ID: "auth-b", Provider: "codex", Attributes: map[string]string{"base_url": server.URL}, Metadata: map[string]any{"account_id": "synthetic-b"}}
					if mode == "same" {
						auth.ID = "renamed-auth-after-refresh"
						auth.Metadata["account_id"] = "synthetic-a"
					}
					if mode == "api-key" {
						auth.Attributes["api_key"] = "synthetic-key"
					}
					var executor cliproxyauth.ProviderExecutor = NewCodexExecutor(cfg)
					if transport == "ws" {
						executor = NewCodexWebsocketsExecutor(cfg)
					}
					if stream {
						result, err := executor.ExecuteStream(ctx, auth, req, opts)
						if err != nil {
							t.Fatal(err)
						}
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								t.Fatal(chunk.Err)
							}
						}
						if mode != "api-key" && result.OnResponseCommitted == nil {
							t.Fatal("missing delivery observer")
						}
					} else {
						result, err := executor.Execute(ctx, auth, req, opts)
						if err != nil {
							t.Fatal(err)
						}
						if mode != "api-key" && result.OnResponseCommitted == nil {
							t.Fatal("missing delivery observer")
						}
					}
					got := <-captured
					gotHeader, gotBody := got[0], got[1]
					wantHeader, wantBody := token, token
					if mode == "foreign" {
						wantHeader, wantBody = "", ""
					}
					if mode == "override" {
						wantHeader = ""
					}
					if gotHeader != wantHeader || gotBody != wantBody {
						t.Fatalf("state header/body = %q/%q, want %q/%q", gotHeader, gotBody, wantHeader, wantBody)
					}
				})
			}
		}
	}
}

func TestCodexMemoryMarkerRequiresExplicitNativeOAuthHeader(t *testing.T) {
	for _, tc := range []struct {
		name string
		auth *cliproxyauth.Auth
		want string
	}{
		{"oauth", &cliproxyauth.Auth{ID: "oauth", Provider: "codex"}, "true"},
		{"api-key", &cliproxyauth.Auth{ID: "apikey", Provider: "codex", Attributes: map[string]string{"api_key": "synthetic"}}, ""},
		{"siwc", &cliproxyauth.Auth{ID: "siwc", Provider: "codex", Attributes: map[string]string{"use_siwc": "true"}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, present := range []bool{false, true} {
				source := http.Header{}
				want := ""
				if present {
					source.Set("X-OpenAI-Memgen-Request", "true")
					want = tc.want
				}
				r, _ := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", nil)
				applyCodexHeaders(r, tc.auth, "", true, nil, source)
				ws := applyCodexWebsocketHeaders(context.Background(), nil, tc.auth, "", nil, true, source)
				if r.Header.Get("X-OpenAI-Memgen-Request") != want || ws.Get("X-OpenAI-Memgen-Request") != want {
					t.Fatal("memory marker was synthesized or propagated outside native OAuth")
				}
			}
		})
	}
}
