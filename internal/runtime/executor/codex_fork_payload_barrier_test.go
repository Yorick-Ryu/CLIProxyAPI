package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/klauspost/compress/zstd"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

// Inspect the wire after account identity, timezone, compression, and framing.
// Removing one array element also detects accidentally applying rules twice.
func TestCodexForkPayloadBarrier(t *testing.T) {
	for _, transport := range []string{"http", "websocket"} {
		for _, stream := range []bool{false, true} {
			t.Run(transport+map[bool]string{false: "/execute", true: "/stream"}[stream], func(t *testing.T) {
				captured := make(chan []byte, 1)
				completed := []byte(`{"type":"response.completed","response":{"id":"resp_fork_barrier","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":0,"total_tokens":1}}}`)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if transport == "websocket" {
						upgrader := websocket.Upgrader{EnableCompression: true}
						conn, err := upgrader.Upgrade(w, r, nil)
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
						captured <- body
						if errWrite := conn.WriteMessage(websocket.TextMessage, completed); errWrite != nil {
							t.Error(errWrite)
						}
						return
					}
					if r.Header.Get("Content-Encoding") != "zstd" {
						t.Error("OAuth request compression was lost")
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
					captured <- body
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = w.Write(append(append([]byte("data: "), completed...), '\n', '\n'))
				}))
				defer server.Close()
				cfg := &config.Config{
					Codex: config.CodexConfig{
						IdentityConvergence: true,
						Timezone:            config.CodexTimezoneConfig{Mode: "fixed", Zone: "Pacific/Honolulu"},
						RequestCompression:  config.CodexRequestCompressionConfig{Enabled: true},
					},
					Payload: config.PayloadConfig{
						Override: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "*"}}, Params: map[string]any{
							"input.1.id":             "configured-id",
							"input.1.content.0.text": "<timezone>UTC</timezone>",
						}}},
						Filter: []config.PayloadFilterRule{{Models: []config.PayloadModelRule{{Name: "*"}}, Params: []string{"input.0", "prompt_cache_key", "client_metadata", "instructions"}}},
					},
				}
				var executor cliproxyauth.ProviderExecutor = NewCodexExecutor(cfg)
				if transport == "websocket" {
					executor = NewCodexWebsocketsExecutor(cfg)
				}
				auth := &cliproxyauth.Auth{ID: "fork-barrier", Provider: "codex", Attributes: map[string]string{"base_url": server.URL}, Metadata: map[string]any{"access_token": "test-token", "account_id": "test-account"}}
				req := cliproxyexecutor.Request{Model: "gpt-5.6-sol", Payload: []byte(`{"model":"gpt-5.6-sol","prompt_cache_key":"client-session","client_metadata":{"session_id":"client-session","x-codex-installation-id":"client-device"},"input":[{"role":"user","content":[{"type":"input_text","text":"first"}]},{"role":"user","content":[{"type":"input_text","text":"<environment_context>\n<timezone>Asia/Shanghai</timezone>\n</environment_context>"}]}]}`)}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatCodex}
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
				case body := <-captured:
					for _, path := range []string{"prompt_cache_key", "client_metadata", "instructions"} {
						if gjson.GetBytes(body, path).Exists() {
							t.Fatalf("filtered field %s was restored: %s", path, body)
						}
					}
					if gjson.GetBytes(body, "input.#").Int() != 1 || gjson.GetBytes(body, "input.0.id").String() != "configured-id" || gjson.GetBytes(body, "input.0.content.0.text").String() != "<timezone>UTC</timezone>" {
						t.Fatalf("payload rules changed after finalization: %s", body)
					}
				default:
					t.Fatal("upstream did not receive a request")
				}
			})
		}
	}
}
