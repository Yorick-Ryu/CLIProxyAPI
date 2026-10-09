package executor

import (
	"context"
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

func TestCodexSearchHistoryOutgoingPayload(t *testing.T) {
	for _, transport := range []string{"http", "ws"} {
		for _, stream := range []bool{false, true} {
			for _, mode := range []string{"standard", "lite", "payload override", "API key"} {
				t.Run(transport+"/"+map[bool]string{true: "stream", false: "complete"}[stream]+"/"+mode, func(t *testing.T) {
					captured := make(chan []byte, 1)
					terminal := []byte(`{"type":"response.completed","response":{"id":"resp_test","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"OK"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`)
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if transport == "ws" {
							conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
							if err != nil {
								t.Error(err)
								return
							}
							defer func() { _ = conn.Close() }()
							_, body, err := conn.ReadMessage()
							if err != nil {
								t.Error(err)
								return
							}
							captured <- body
							if err = conn.WriteMessage(websocket.TextMessage, terminal); err != nil {
								t.Error(err)
							}
						} else {
							body, err := io.ReadAll(r.Body)
							if err != nil {
								t.Error(err)
								return
							}
							captured <- body
							w.Header().Set("Content-Type", "text/event-stream")
							_, _ = w.Write(append(append([]byte("data: "), terminal...), []byte("\n\n")...))
						}
					}))
					defer server.Close()
					cfg := &config.Config{}
					cfg.DisableImageGeneration = config.DisableImageGenerationChat
					if mode == "payload override" {
						cfg.Payload.Override = []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "*"}}, Params: map[string]any{"tools": []any{}, "tool_choice": "required"}}}
					}
					var executor interface {
						Execute(context.Context, *cliproxyauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error)
						ExecuteStream(context.Context, *cliproxyauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error)
					}
					if transport == "ws" {
						executor = NewCodexWebsocketsExecutor(cfg)
					} else {
						executor = NewCodexExecutor(cfg)
					}
					auth := &cliproxyauth.Auth{ID: "synthetic-account", Provider: "codex", Attributes: map[string]string{"base_url": server.URL}, Metadata: map[string]any{"access_token": "synthetic"}}
					if mode == "API key" {
						auth.Attributes["api_key"] = "synthetic"
					}
					headers := http.Header{}
					if mode == "lite" {
						headers.Set(codexResponsesLiteHeader, "true")
					}
					payload := []byte(`{"model":"gpt-5.5","input":[{"type":"web_search_call","id":"ws_test","status":"completed","action":{"type":"search","query":"example"}},{"type":"message","role":"user","content":"Reply OK"},{"type":"compaction_trigger"}],"tools":[],"tool_choice":"auto"}`)
					req := cliproxyexecutor.Request{Model: "gpt-5.5", Payload: payload}
					opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response"), Headers: headers, Stream: stream}
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
					} else {
						if _, err := executor.Execute(context.Background(), auth, req, opts); err != nil {
							t.Fatal(err)
						}
					}
					body := <-captured
					if mode == "payload override" {
						if len(gjson.GetBytes(body, "tools").Array()) != 0 || gjson.GetBytes(body, "tool_choice").String() != "required" {
							t.Fatal("built-in repair overrode final user payload rules")
						}
						return
					}
					if mode == "API key" {
						if gjson.GetBytes(body, `tools.#(type=="web_search")`).Exists() {
							t.Fatal("API key changed")
						}
						return
					}
					path := `tools.#(type=="web_search")`
					if mode == "lite" {
						path = `input.#(type=="additional_tools").tools.#(type=="web_search")`
						if gjson.GetBytes(body, `tools.#(type=="web_search")`).Exists() {
							t.Fatal("wrong Lite placement")
						}
					}
					if !gjson.GetBytes(body, path).Exists() || gjson.GetBytes(body, "tool_choice").String() != "none" {
						t.Fatal("missing repair on outgoing wire")
					}
					items := gjson.GetBytes(body, "input").Array()
					if items[len(items)-1].Get("type").String() != "compaction_trigger" {
						t.Fatal("trigger no longer last")
					}
				})
			}
		}
	}
}

func TestCodexSearchHistoryDoesNotChangeCompactEndpoint(t *testing.T) {
	var captured []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		if r.URL.Path != "/responses/compact" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_test","object":"response.compaction","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()
	auth := &cliproxyauth.Auth{Provider: "codex", Attributes: map[string]string{"base_url": server.URL}, Metadata: map[string]any{"access_token": "synthetic"}}
	_, err := NewCodexExecutor(&config.Config{}).Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: []byte(`{"model":"gpt-5.5","input":[{"type":"web_search_call","status":"completed","action":{"type":"search","query":"example"}}],"tools":[],"tool_choice":"auto"}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response"), Alt: "responses/compact"})
	if err != nil {
		t.Fatal(err)
	}
	if len(gjson.GetBytes(captured, "tools").Array()) != 0 || gjson.GetBytes(captured, "tool_choice").String() != "auto" {
		t.Fatal("changed separate compact endpoint")
	}
}
