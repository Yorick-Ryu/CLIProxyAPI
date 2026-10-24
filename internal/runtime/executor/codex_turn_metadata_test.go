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
	"time"

	"github.com/gorilla/websocket"
	"github.com/klauspost/compress/zstd"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCodexTurnMetadataSerializationOnWire(t *testing.T) {
	const metadata = `{"z_extension":"<>&東京🚀\b\f","analytics_enabled":true,"turn_started_at_unix_ms":9007199254740993,"history_initialization":"new","request_kind":"turn","turn_id":"019a0992-1470-7abc-8def-0123456789ab","thread_id":"019a0992-1400-7abc-8def-0123456789ab","session_id":"019a0992-1400-7abc-8def-0123456789ab","installation_id":"client-device"}`
	const override = ` {"z_extension":"override","analytics_enabled":true,"agent_name":"explicit"} `
	for _, transport := range []string{"http", "ws"} {
		for _, stream := range []bool{false, true} {
			for _, mode := range []string{"oauth", "payload-override", "payload-filter", "api-key"} {
				t.Run(fmt.Sprintf("%s/stream%v/%s", transport, stream, mode), func(t *testing.T) {
					type capture struct {
						headers http.Header
						body    []byte
					}
					captured := make(chan capture, 1)
					completed := []byte(`{"type":"response.completed","response":{"id":"resp_metadata","status":"completed","output":[]}}`)
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if transport == "ws" {
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
							captured <- capture{r.Header.Clone(), body}
							if errWrite := conn.WriteMessage(websocket.TextMessage, completed); errWrite != nil {
								t.Error(errWrite)
							}
							return
						}
						var reader io.Reader = r.Body
						if r.Header.Get("Content-Encoding") == "zstd" {
							decoder, err := zstd.NewReader(r.Body)
							if err != nil {
								t.Error(err)
								return
							}
							defer decoder.Close()
							reader = decoder
						} else if mode != "api-key" {
							t.Error("OAuth request lost zstd compression")
						}
						body, errRead := io.ReadAll(reader)
						if errRead != nil {
							t.Error(errRead)
							return
						}
						captured <- capture{r.Header.Clone(), body}
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = fmt.Fprintf(w, "data: %s\n\n", completed)
					}))
					defer server.Close()
					cfg := &config.Config{
						SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll},
						Codex:     config.CodexConfig{IdentityConvergence: true, RequestCompression: config.CodexRequestCompressionConfig{Enabled: true}},
					}
					if mode == "payload-override" {
						cfg.Payload.Override = []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "*"}}, Params: map[string]any{"client_metadata.x-codex-turn-metadata": override}}}
					} else if mode == "payload-filter" {
						cfg.Payload.Filter = []config.PayloadFilterRule{{Models: []config.PayloadModelRule{{Name: "*"}}, Params: []string{"client_metadata.x-codex-turn-metadata"}}}
					}
					auth := &cliproxyauth.Auth{ID: "metadata-wire", Provider: "codex", Attributes: map[string]string{"base_url": server.URL}, Metadata: map[string]any{"account_id": "synthetic-account", "access_token": "synthetic-token"}}
					if mode == "api-key" {
						auth.Attributes["api_key"] = "synthetic-key"
					}
					body, err := json.Marshal(map[string]any{"model": "gpt-6-astra", "input": []any{}, "client_metadata": map[string]any{"x-codex-turn-metadata": metadata}})
					if err != nil {
						t.Fatal(err)
					}
					var executor cliproxyauth.ProviderExecutor = NewCodexExecutor(cfg)
					if transport == "ws" {
						executor = NewCodexWebsocketsExecutor(cfg)
					}
					req := cliproxyexecutor.Request{Model: "gpt-6-astra", Payload: body}
					opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatCodex, Headers: http.Header{"X-Codex-Turn-Metadata": {metadata}}}
					if stream {
						result, errExecute := executor.ExecuteStream(context.Background(), auth, req, opts)
						if errExecute != nil {
							t.Fatal(errExecute)
						}
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								t.Fatal(chunk.Err)
							}
						}
					} else if _, errExecute := executor.Execute(context.Background(), auth, req, opts); errExecute != nil {
						t.Fatal(errExecute)
					}
					got := <-captured
					embedded := gjson.GetBytes(got.body, "client_metadata.x-codex-turn-metadata")
					if mode == "api-key" {
						if embedded.String() != metadata || got.headers.Get("X-Codex-Turn-Metadata") != metadata {
							t.Fatal("API-key metadata was reformatted")
						}
						return
					}
					actual := got.headers.Get("X-Codex-Turn-Metadata")
					parsed := gjson.Parse(actual)
					want := fmt.Sprintf(`{"installation_id":%q,"session_id":%q,"thread_id":%q,"turn_id":%q,"request_kind":"turn","history_initialization":"new","turn_started_at_unix_ms":9007199254740993,"analytics_enabled":true,"z_extension":"<>&\u6771\u4eac\ud83d\ude80\b\f"}`, parsed.Get("installation_id").String(), parsed.Get("session_id").String(), parsed.Get("thread_id").String(), parsed.Get("turn_id").String())
					if actual != want || !strings.Contains(actual, `"turn_id":"019a0992-1470-7`) {
						t.Fatalf("header lost native formatting or the UUIDv7 timestamp\ngot: %q\nwant: %q", actual, want)
					}
					switch mode {
					case "payload-override":
						if embedded.String() != override {
							t.Fatal("serialization changed an explicit payload override")
						}
					case "payload-filter":
						if embedded.Exists() {
							t.Fatal("serialization restored filtered metadata")
						}
					default:
						if embedded.String() != actual {
							t.Fatal("header and embedded metadata serialize differently")
						}
					}
				})
			}
		}
	}
}

func TestCodexDuplexTurnMetadataSerialization(t *testing.T) {
	const original = `{"z":"<>&東京","agent_name":"steering"}`
	const canonical = `{"agent_name":"steering","z":"<>&\u6771\u4eac"}`
	const override = ` {"z":"explicit","agent_name":"override"} `
	for _, mode := range []string{"oauth", "payload-override", "payload-filter", "api-key"} {
		t.Run(mode, func(t *testing.T) {
			captured := make(chan []byte, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer func() { _ = conn.Close() }()
				_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
				if _, _, err = conn.ReadMessage(); err != nil {
					t.Error(err)
					return
				}
				if err = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"r1","output":[]}}`)); err != nil {
					t.Error(err)
					return
				}
				_, payload, err := conn.ReadMessage()
				if err != nil {
					t.Error(err)
					return
				}
				captured <- payload
				if err = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"r1","status":"completed","output":[]}}`)); err != nil {
					t.Error(err)
					return
				}
				_, _, _ = conn.ReadMessage()
			}))
			defer server.Close()
			cfg := &config.Config{Codex: config.CodexConfig{ResponseSteering: true}}
			if mode == "payload-override" {
				cfg.Payload.Override = []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "*"}}, Params: map[string]any{"client_metadata.x-codex-turn-metadata": override}}}
			} else if mode == "payload-filter" {
				cfg.Payload.Filter = []config.PayloadFilterRule{{Models: []config.PayloadModelRule{{Name: "*"}}, Params: []string{"client_metadata.x-codex-turn-metadata"}}}
			}
			executor := NewCodexWebsocketsExecutor(cfg)
			executor.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
			defer executor.CloseExecutionSession("metadata-duplex")
			auth := &cliproxyauth.Auth{ID: "metadata-duplex-auth", Provider: "codex", Attributes: map[string]string{"base_url": server.URL}, Metadata: map[string]any{"access_token": "synthetic-token"}}
			if mode == "api-key" {
				auth.Attributes["api_key"] = "synthetic-key"
			}
			payload, err := json.Marshal(map[string]any{"type": "response.steer", "previous_response_id": "r1", "input": "steer", "client_metadata": map[string]any{"x-codex-turn-metadata": original}})
			if err != nil {
				t.Fatal(err)
			}
			input := make(chan cliproxyexecutor.WebsocketInput, 1)
			input <- cliproxyexecutor.WebsocketInput{Payload: payload}
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			ctx = cliproxyexecutor.WithWebsocketInput(cliproxyexecutor.WithDownstreamWebsocket(ctx), input)
			req := cliproxyexecutor.Request{Model: "gpt-6-astra", Payload: []byte(`{"model":"gpt-6-astra","input":[]}`)}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatCodex, Metadata: map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: "metadata-duplex"}}
			result, err := executor.ExecuteStream(ctx, auth, req, opts)
			if err != nil {
				t.Fatal(err)
			}
			for chunk := range result.Chunks {
				if chunk.Err != nil {
					t.Fatal(chunk.Err)
				}
				if gjson.GetBytes(chunk.Payload, "type").String() == "response.completed" {
					break
				}
			}
			select {
			case got := <-captured:
				metadata := gjson.GetBytes(got, "client_metadata.x-codex-turn-metadata")
				want := canonical
				if mode == "api-key" {
					want = original
				} else if mode == "payload-override" {
					want = override
				}
				if mode == "payload-filter" {
					if metadata.Exists() {
						t.Fatal("steering serialization restored filtered metadata")
					}
				} else if metadata.String() != want {
					t.Fatalf("steering metadata = %q, want %q", metadata.String(), want)
				}
				if gjson.GetBytes(got, "type").String() != "response.steer" || gjson.GetBytes(got, "input").String() != "steer" {
					t.Fatal("serialization changed steering control or input")
				}
			case <-ctx.Done():
				t.Fatal("upstream did not receive steering")
			}
		})
	}
}

func TestCodexTurnMetadataFormattingAuthScope(t *testing.T) {
	const metadata = `{"z":"keep","agent_name":"agent"}`
	const canonical = `{"agent_name":"agent","z":"keep"}`
	cfg := &config.Config{Codex: config.CodexConfig{IdentityConfuse: true}}
	cfg.Routing.Strategy = "fill-first"
	body := []byte(`{"client_metadata":{"x-codex-turn-metadata":` + fmt.Sprintf("%q", metadata) + `}}`)
	for _, apiKey := range []bool{false, true} {
		auth := &cliproxyauth.Auth{ID: "scope-fixture", Provider: "codex", Attributes: map[string]string{}}
		if apiKey {
			auth.Attributes["api_key"] = "synthetic-key"
		}
		_, state := applyCodexIdentityConfuseBody(cfg, auth, body, body)
		if !state.enabled {
			t.Fatal("fixture must cover the legacy identity-confuse path")
		}
		headers := http.Header{"X-Codex-Turn-Metadata": {metadata}}
		applyCodexIdentityConfuseHeaders(headers, &state)
		want := canonical
		if apiKey {
			want = metadata
		}
		if got := headers.Get("X-Codex-Turn-Metadata"); got != want {
			t.Fatalf("apiKey=%v: metadata = %q, want %q", apiKey, got, want)
		}
	}
}
