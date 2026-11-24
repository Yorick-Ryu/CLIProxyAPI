package cliproxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestCodexRetryPolicyConfigReload(t *testing.T) {
	ctx := context.Background()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "326")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"code":"server_is_overloaded","type":"service_unavailable_error"}}`)
	}))
	t.Cleanup(upstream.Close)

	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{
		ID: "codex-retry-reload", Provider: "codex",
		Attributes: map[string]string{"api_key": "test", "base_url": upstream.URL},
	}
	if _, err := manager.Register(coreauth.WithSkipPersist(ctx), auth); err != nil {
		t.Fatal(err)
	}
	service := &Service{cfg: &config.Config{}, coreManager: manager}
	req := cliproxyexecutor.Request{
		Model: "gpt-6.1-sol", Payload: []byte(`{"model":"gpt-6.1-sol","input":[{"role":"user","content":"test"}]}`),
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")}

	for _, respect := range []bool{true, false, true} {
		cfg := &config.Config{TransientErrorRespectRetryAfter: &respect}
		if !service.applyConfigUpdateWithAuthSynthesis(ctx, cfg, false) {
			t.Fatal("reload failed")
		}
		executor, ok := manager.Executor("codex")
		if !ok {
			t.Fatal("missing Codex executor")
		}
		_, err := executor.Execute(ctx, auth, req, opts)
		var rejection interface {
			StatusCode() int
			RetryAfter() *time.Duration
		}
		if !errors.As(err, &rejection) || rejection.StatusCode() != http.StatusServiceUnavailable {
			t.Fatalf("expected 503, got %v", err)
		}
		got := rejection.RetryAfter()
		if respect && (got == nil || *got != 326*time.Second) || !respect && got != nil {
			t.Fatalf("respect=%t: unexpected Retry-After %v", respect, got)
		}
	}
}
