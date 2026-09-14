package executor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	auth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	exec "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	translator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func TestCodexAutoWebsocketsDefaultTransport(t *testing.T) {
	on, off := true, false
	for _, tc := range []struct {
		name                 string
		global, account      *bool
		downstreamWS, wantWS bool
	}{
		{"omitted", nil, nil, true, true},
		{"global enabled", &on, nil, true, true},
		{"global disabled", &off, nil, true, false},
		{"account disables default", nil, &off, true, false},
		{"account disables global", &on, &off, true, false},
		{"account enables global", &off, &on, true, true},
		{"HTTP stays HTTP", nil, nil, false, false},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				observed := make(chan bool, 2)
				event := []byte(`{"type":"response.completed","response":{"id":"resp-test","object":"response","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`)
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					isWS := websocket.IsWebSocketUpgrade(r)
					observed <- isWS
					if isWS {
						upgrader := websocket.Upgrader{}
						conn, err := upgrader.Upgrade(w, r, nil)
						if err != nil {
							t.Error(err)
							return
						}
						defer conn.Close()
						if _, _, err := conn.ReadMessage(); err != nil {
							t.Error(err)
							return
						}
						if err := conn.WriteMessage(websocket.TextMessage, event); err != nil {
							t.Error(err)
						}
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprintf(w, "data: %s\n\n", event)
				}))
				defer upstream.Close()
				cfg := &config.Config{Codex: config.CodexConfig{WebsocketsDefault: tc.global}}
				executor := NewCodexAutoExecutor(cfg)
				credential := &auth.Auth{Provider: "codex", Attributes: map[string]string{"base_url": upstream.URL, "api_key": "test"}}
				if tc.account != nil {
					credential.Metadata = map[string]any{"websockets": *tc.account}
				}
				ctx := context.Background()
				if tc.downstreamWS {
					ctx = exec.WithDownstreamWebsocket(ctx)
				}
				req := exec.Request{Model: "gpt-5.6-sol", Payload: []byte(`{"model":"gpt-5.6-sol","input":"hello"}`)}
				opts := exec.Options{SourceFormat: translator.FromString("openai-response"), ResponseFormat: translator.FromString("openai-response"), Stream: stream}
				if stream {
					result, err := executor.ExecuteStream(ctx, credential, req, opts)
					if err != nil {
						t.Fatal(err)
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
					}
				} else {
					if _, err := executor.Execute(ctx, credential, req, opts); err != nil {
						t.Fatal(err)
					}
				}
				select {
				case got := <-observed:
					if got != tc.wantWS {
						t.Fatalf("upstream websocket = %t, want %t", got, tc.wantWS)
					}
				default:
					t.Fatal("no upstream request")
				}
			})
		}
	}
}
