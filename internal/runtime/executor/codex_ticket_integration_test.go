package executor

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	auth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	exec "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	translator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

type ticketIntegrationTransport func(*http.Request) (*http.Response, error)

func (f ticketIntegrationTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCodexTurnTicketHTTPExecutorInjection(t *testing.T) {
	for _, stream := range []bool{false, true} {
		cfg := &config.Config{}
		cfg.Codex.TurnStateTicket = config.CodexTurnStateTicketConfig{Enabled: true, Models: []string{"gpt-5.6-luna"}}
		a := &auth.Auth{ID: "ticket-integration", Provider: "codex", Metadata: map[string]any{"access_token": "test", "account_id": "ticket-integration-account"}}
		value := "gAAAAA" + strings.Repeat("B", 286)
		if !helps.DefaultCodexTurnTickets.Capture(cfg, a, "gpt-5.6-luna", 200, value) {
			t.Fatal("capture failed")
		}
		seen := false
		rt := ticketIntegrationTransport(func(r *http.Request) (*http.Response, error) {
			seen = true
			if r.Header.Get("X-Codex-Turn-State") != value {
				t.Error("executor did not inject captured ticket")
			}
			body := `data: {"type":"response.completed","response":{"id":"resp-test","object":"response","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n"
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
		})
		ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", rt)
		executor := NewCodexExecutor(cfg)
		req := exec.Request{Model: "gpt-5.6-luna", Payload: []byte(`{"model":"gpt-5.6-luna","input":"hello"}`)}
		opts := exec.Options{SourceFormat: translator.FromString("openai-response"), ResponseFormat: translator.FromString("openai-response"), Stream: stream}
		if stream {
			response, err := executor.ExecuteStream(ctx, a, req, opts)
			if err != nil {
				t.Fatal(err)
			}
			for chunk := range response.Chunks {
				if chunk.Err != nil {
					t.Fatal(chunk.Err)
				}
			}
		} else if _, err := executor.Execute(ctx, a, req, opts); err != nil {
			t.Fatal(err)
		}
		if !seen {
			t.Fatal("no upstream request")
		}
	}
}
