package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/translator"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

// Exercise the real handler, auth manager, Codex transports and disconnect notifier.
func TestResponsesWebsocketCodexSizeHTTPFallback(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, scenario := range []string{"prewarm", "local", "close1009", "error413", "after_event", "http_failure", "delta", "full_after_native", "normal"} {
		t.Run(scenario, func(t *testing.T) {
			var wsCalls, httpCalls atomic.Int32
			bodies := make(chan []byte, 4)
			ack := make(chan struct{})
			completed := func(id string) string {
				return fmt.Sprintf(`{"type":"response.completed","response":{"id":%q,"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"remembered"}]}],"usage":{"input_tokens":10,"output_tokens":1,"total_tokens":11}}}`, id)
			}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !websocket.IsWebSocketUpgrade(r) {
					n := httpCalls.Add(1)
					body, _ := io.ReadAll(r.Body)
					bodies <- body
					if scenario == "http_failure" {
						w.WriteHeader(http.StatusRequestEntityTooLarge)
						_, _ = io.WriteString(w, `{"error":{"code":"message_too_big","message":"HTTP limit"}}`)
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprintf(w, "data: %s\n\n", completed(fmt.Sprintf("http-%d", n)))
					return
				}
				wsCalls.Add(1)
				conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer func() { _ = conn.Close() }()
				if _, _, err = conn.ReadMessage(); err != nil {
					return
				}
				if scenario == "normal" || scenario == "delta" || scenario == "full_after_native" {
					_ = conn.WriteMessage(websocket.TextMessage, []byte(completed("ws-1")))
					if _, _, err = conn.ReadMessage(); err != nil {
						return
					}
					if scenario == "normal" {
						_ = conn.WriteMessage(websocket.TextMessage, []byte(completed("ws-2")))
						_, _, _ = conn.ReadMessage()
						return
					}
				}
				if scenario == "after_event" {
					_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"started"}}`))
					select {
					case <-ack:
					case <-r.Context().Done():
						return
					}
				}
				if scenario == "error413" {
					_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","status":413,"error":{"type":"invalid_request_error","code":"message_too_big","message":"too big"}}`))
					// Let the executor consume the error before the socket disappears.
					_, _, _ = conn.ReadMessage()
					return
				}
				_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1009, "too big"), time.Now().Add(time.Second))
			}))
			defer upstream.Close()
			cfg := &config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}}
			if scenario == "local" || scenario == "prewarm" {
				cfg.Codex.WebsocketMaxMessageBytes = 4000
			}
			manager := coreauth.NewManager(nil, nil, nil)
			manager.SetConfig(cfg)
			manager.RegisterExecutor(runtimeexecutor.NewCodexAutoExecutor(cfg))
			authID := "size-fallback-" + scenario
			_, err := manager.Register(context.Background(), &coreauth.Auth{ID: authID, Provider: "codex", Status: coreauth.StatusActive, Attributes: map[string]string{"api_key": "fixture", "base_url": upstream.URL, "websockets": "true"}})
			if err != nil {
				t.Fatal(err)
			}
			registry.GetGlobalRegistry().RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: "gpt-5.4"}})
			defer registry.GetGlobalRegistry().UnregisterClient(authID)
			h := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&cfg.SDKConfig, manager))
			router := gin.New()
			router.GET("/v1/responses", h.ResponsesWebsocket)
			server := httptest.NewServer(router)
			defer server.Close()
			conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
			input := "original context"
			if scenario == "local" || scenario == "prewarm" {
				input += strings.Repeat("x", 8000)
			}
			request, _ := json.Marshal(map[string]any{"type": "response.create", "model": "gpt-5.4", "input": []any{map[string]any{"role": "user", "content": input}}})
			if scenario == "prewarm" {
				request = []byte(strings.Replace(string(request), `"type":"response.create"`, `"type":"response.create","generate":false`, 1))
			}
			if err = conn.WriteMessage(websocket.TextMessage, request); err != nil {
				t.Fatal(err)
			}
			readCompleted := func() string {
				_, data, readErr := conn.ReadMessage()
				if readErr != nil {
					t.Fatalf("expected completion: %v", readErr)
				}
				if gjson.GetBytes(data, "type").String() != "response.completed" {
					t.Fatalf("unexpected event: %s", data)
				}
				return gjson.GetBytes(data, "response.id").String()
			}
			if scenario == "after_event" {
				_, data, readErr := conn.ReadMessage()
				if readErr != nil || gjson.GetBytes(data, "type").String() != "response.created" {
					t.Fatalf("missing created: %s %v", data, readErr)
				}
				close(ack)
			}
			if scenario == "delta" {
				id := readCompleted()
				_ = conn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`{"type":"response.create","model":"gpt-5.4","previous_response_id":%q,"input":[{"role":"user","content":"delta"}]}`, id)))
			}
			if scenario == "full_after_native" {
				readCompleted()
				if err = conn.WriteMessage(websocket.TextMessage, request); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "after_event" || scenario == "delta" || scenario == "http_failure" {
				_, data, readErr := conn.ReadMessage()
				if scenario == "http_failure" && readErr == nil && gjson.GetBytes(data, "type").String() == "error" {
					_, _, readErr = conn.ReadMessage()
				}
				if readErr == nil {
					t.Fatal("expected terminal close")
				}
				wantHTTP := int32(0)
				if scenario == "http_failure" {
					wantHTTP = 1
				}
				if httpCalls.Load() != wantHTTP {
					t.Fatalf("HTTP attempts = %d, want %d", httpCalls.Load(), wantHTTP)
				}
				return
			}
			if scenario == "prewarm" {
				var id string
				for {
					_, event, e := conn.ReadMessage()
					if e != nil {
						t.Fatal(e)
					}
					if gjson.GetBytes(event, "type").String() == "response.completed" {
						id = gjson.GetBytes(event, "response.id").String()
						break
					}
				}
				if httpCalls.Load() != 0 || wsCalls.Load() != 0 {
					t.Fatal("prewarm generated or dialed upstream")
				}
				_ = conn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`{"type":"response.create","model":"gpt-5.4","previous_response_id":%q,"input":[{"role":"user","content":"generate now"}]}`, id)))
			}
			id := readCompleted()
			_ = conn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`{"type":"response.create","model":"gpt-5.4","previous_response_id":%q,"input":[{"role":"user","content":"followup"}]}`, id)))
			readCompleted()
			if scenario == "normal" {
				if httpCalls.Load() != 0 || wsCalls.Load() != 1 {
					t.Fatal("normal incremental transport changed")
				}
				return
			}
			if httpCalls.Load() != 2 {
				t.Fatalf("HTTP calls = %d, want 2", httpCalls.Load())
			}
			wantWS := int32(1)
			if scenario == "local" || scenario == "prewarm" {
				wantWS = 0
			}
			if wsCalls.Load() != wantWS {
				t.Fatalf("WS calls = %d, want %d", wsCalls.Load(), wantWS)
			}
			first, second := <-bodies, <-bodies
			if !strings.Contains(string(first), input) {
				t.Fatal("HTTP replay lost original input")
			}
			for _, text := range []string{input, "remembered", "followup"} {
				if !strings.Contains(string(second), text) {
					t.Fatalf("continuation lost %q", text[:min(len(text), 30)])
				}
			}
			if gjson.GetBytes(second, "previous_response_id").Exists() {
				t.Fatal("HTTP continuation retained upstream-only reference")
			}
		})
	}
}
