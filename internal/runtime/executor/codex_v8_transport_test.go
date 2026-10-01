package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestCodexV8TransportScopeAgreement(t *testing.T) {
	for _, layout := range []string{"legacy", "v8"} {
		t.Run(layout, func(t *testing.T) {
			raw := []byte("codex: {websockets-default: false}\n")
			if layout == "v8" {
				var err error
				raw, _, err = config.NormalizeConfigLayout(raw, true)
				if err != nil {
					t.Fatal(err)
				}
			}
			cfg, err := config.ParseConfigBytes(raw)
			if err != nil {
				t.Fatal(err)
			}
			observed := make(chan bool, 2)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				observed <- websocket.IsWebSocketUpgrade(r)
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":{"message":"test rejection"}}`))
			}))
			defer upstream.Close()
			manager := cliproxyauth.NewManager(nil, nil, nil)
			manager.SetConfig(cfg)
			credential, err := manager.Register(context.Background(), &cliproxyauth.Auth{
				ID: "transport-scope-key", Provider: "codex", ProxyURL: "direct",
				Attributes: map[string]string{"api_key": "test", "base_url": upstream.URL},
			})
			if err != nil {
				t.Fatal(err)
			}
			bound := NewCodexAutoExecutor(cfg).ForAPIKey()
			for _, stream := range []bool{false, true} {
				ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
				req := cliproxyexecutor.Request{Model: "gpt-5.6-sol", Payload: []byte(`{"model":"gpt-5.6-sol","input":"hello"}`)}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Stream: stream}
				if stream {
					_, err = bound.ExecuteStream(ctx, credential, req, opts)
				} else {
					_, err = bound.Execute(ctx, credential, req, opts)
				}
				if err == nil {
					t.Fatal("expected upstream rejection")
				}
				select {
				case actual := <-observed:
					if actual != credential.EffectiveWebsocketsEnabled() || actual != (layout == "v8") {
						t.Fatalf("scheduler/handler websocket=%t, actual upstream websocket=%t (stream=%t)", credential.EffectiveWebsocketsEnabled(), actual, stream)
					}
				default:
					t.Fatal("no upstream request")
				}
			}
		})
	}
}
