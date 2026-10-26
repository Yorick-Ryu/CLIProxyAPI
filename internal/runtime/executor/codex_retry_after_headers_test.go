package executor

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	auth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	core "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	translator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

const codexTurnRateLimitTestBody = `{"error":{"type":"rate_limit_error","code":"turn_rate_limit_exceeded","message":"Codex is at capacity, please try again later"}}`

func requireCodexRetryHint(t *testing.T, err error, want time.Duration) {
	t.Helper()
	var retry interface{ RetryAfter() *time.Duration }
	if !errors.As(err, &retry) || retry.RetryAfter() == nil || *retry.RetryAfter() != want {
		t.Fatalf("retry delay for %v = %v, want %v", err, retry, want)
	}
}

func TestCodexHTTPRetryAfterReachesExecutors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "13")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(codexTurnRateLimitTestBody))
	}))
	defer server.Close()
	cfg := &config.Config{}
	credential := &auth.Auth{ID: "codex-retry-hint", Provider: "codex", Attributes: map[string]string{"api_key": "test", "base_url": server.URL}}
	req := core.Request{Model: "gpt-6-luna", Payload: []byte(`{"model":"gpt-6-luna","input":[]}`)}
	opts := core.Options{SourceFormat: translator.FromString("codex")}
	cases := []struct {
		name string
		call func() error
	}{
		{"http", func() error {
			_, err := NewCodexExecutor(cfg).Execute(context.Background(), credential, req, opts)
			return err
		}},
		{"http_stream", func() error {
			_, err := NewCodexExecutor(cfg).ExecuteStream(context.Background(), credential, req, opts)
			return err
		}},
		{"compact", func() error {
			o := opts
			o.Alt = "responses/compact"
			_, err := NewCodexExecutor(cfg).Execute(context.Background(), credential, req, o)
			return err
		}},
		{"ws_handshake", func() error {
			_, err := NewCodexWebsocketsExecutor(cfg).Execute(context.Background(), credential, req, opts)
			return err
		}},
		{"ws_stream_handshake", func() error {
			_, err := NewCodexWebsocketsExecutor(cfg).ExecuteStream(context.Background(), credential, req, opts)
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { requireCodexRetryHint(t, tc.call(), 13*time.Second) })
	}
}

func TestCodexWebsocketFrameRetryAfter(t *testing.T) {
	for _, body := range []string{
		`{"type":"error","status":429,"error":{"type":"rate_limit_error","code":"turn_rate_limit_exceeded"},"headers":{"Retry-After":"13"}}`,
		`{"type":"error","status":429,"headers":{"retry-after":"27"},"error":{"type":"rate_limit_error","headers":{"retry-after":"13"}}}`,
		`{"type":"error","status":429,"headers":{"retry-after":"13"},"error":{"type":"rate_limit_error","headers":{"retry-after":"invalid"}}}`,
		`{"type":"error","status":429,"error":{"type":"rate_limit_error","headers":{"retry-after":13}}}`,
		`{"type":"error","status":429,"body":{"error":{"type":"rate_limit_error"},"headers":{"retry-after":["13"]}}}`,
	} {
		t.Run(body, func(t *testing.T) {
			err, ok := parseCodexWebsocketError([]byte(body))
			if !ok {
				t.Fatal("not recognized")
			}
			requireCodexRetryHint(t, err, 13*time.Second)
		})
	}
}

func TestCodexRetryAfterCoolsModelAndPreventsEarlyRedispatch(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "13")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(codexTurnRateLimitTestBody))
	}))
	defer server.Close()
	cfg := &config.Config{}
	manager := auth.NewManager(nil, nil, nil)
	manager.SetConfig(cfg)
	manager.SetRetryConfig(0, 0, 0)
	manager.RegisterExecutor(NewCodexExecutor(cfg))
	id, model := "codex-retry-after-scheduler", "codex-retry-after-model"
	_, err := manager.Register(context.Background(), &auth.Auth{ID: id, Provider: "codex", Status: auth.StatusActive, Attributes: map[string]string{"api_key": "test", "base_url": server.URL}})
	if err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(id, "codex", []*registry.ModelInfo{{ID: model}})
	defer registry.GetGlobalRegistry().UnregisterClient(id)
	req := core.Request{Model: model, Payload: []byte(`{"input":[]}`)}
	opts := core.Options{SourceFormat: translator.FromString("codex")}
	before := time.Now()
	_, err = manager.ExecuteStream(context.Background(), []string{"codex"}, req, opts)
	if err == nil {
		t.Fatal("wanted rejection")
	}
	current, ok := manager.GetByID(id)
	if !ok {
		t.Fatal("account missing")
	}
	state := current.ModelStates[model]
	if state == nil || state.NextRetryAfter.Before(before.Add(13*time.Second)) {
		t.Fatalf("upstream 13s delay not applied: %+v", state)
	}
	_, _ = manager.ExecuteStream(context.Background(), []string{"codex"}, req, opts)
	if calls.Load() != 1 {
		t.Fatalf("cooled model dispatched early: %d upstream calls", calls.Load())
	}
}

func TestCodexRetryAfterPreservesQuotaResetAndFallbacks(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	quota := []byte(`{"error":{"type":"usage_limit_reached","resets_in_seconds":3600}}`)
	for _, tc := range []struct {
		name   string
		status int
		body   []byte
		header string
		want   *time.Duration
	}{
		{"captured_capacity_13s", 429, []byte(codexTurnRateLimitTestBody), "13", durationPointer(13 * time.Second)},
		{"quota_longer_than_header", 429, quota, "13", durationPointer(time.Hour)},
		{"header_longer_than_quota", 429, quota, "7200", durationPointer(2 * time.Hour)},
		{"invalid_header_preserves_quota", 429, quota, "invalid", durationPointer(time.Hour)},
		{"invalid_header_preserves_capacity_fallback", 429, []byte(codexTurnRateLimitTestBody), "-3", nil},
		{"zero_is_explicit", 429, []byte(codexTurnRateLimitTestBody), "0", durationPointer(0)},
		{"date", 429, []byte(codexTurnRateLimitTestBody), now.Add(23 * time.Second).Format(http.TimeFormat), durationPointer(23 * time.Second)},
		{"overload", 503, []byte(`{"error":{"code":"server_is_overloaded"}}`), "19", durationPointer(19 * time.Second)},
		{"auth_error_not_shortened", 401, []byte(`{}`), "13", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseCodexRetryAfter(tc.status, tc.body, now, http.Header{"Retry-After": {tc.header}})
			if tc.want == nil {
				if got != nil {
					t.Fatalf("got %v, want nil", *got)
				}
				return
			}
			if got == nil || *got != *tc.want {
				t.Fatalf("got %v, want %v", got, *tc.want)
			}
		})
	}
}

func TestCodexTerminalEventRetryAfter(t *testing.T) {
	for _, event := range []string{
		`{"type":"error","error":{"type":"rate_limit_error","code":"turn_rate_limit_exceeded"},"headers":{"retry-after":"13"}}`,
		`{"type":"response.failed","response":{"error":{"type":"rate_limit_error","code":"turn_rate_limit_exceeded"}},"headers":{"retry-after":"13"}}`,
		`{"type":"response.failed","response":{"error":{"type":"rate_limit_error","code":"turn_rate_limit_exceeded","headers":{"retry-after":"13"}}}}`,
	} {
		err, _, ok := codexTerminalFailureErr([]byte(event))
		if !ok {
			t.Fatal("terminal failure not recognized")
		}
		requireCodexRetryHint(t, err, 13*time.Second)
	}
}
