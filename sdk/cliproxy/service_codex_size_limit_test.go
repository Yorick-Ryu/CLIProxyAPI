package cliproxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func TestCodexWebsocketSizeLimitConfigReload(t *testing.T) {
	var dials atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dials.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer upstream.Close()
	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{ID: "size-limit-reload", Provider: "codex", Attributes: map[string]string{"api_key": "test", "base_url": upstream.URL}}
	if _, err := manager.Register(coreauth.WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatal(err)
	}
	service := &Service{cfg: &config.Config{}, coreManager: manager}
	req := cliproxyexecutor.Request{Model: "gpt-5-codex", Payload: []byte(`{"model":"gpt-5-codex","input":[{"role":"user","content":"` + strings.Repeat("x", 1024) + `"}]}`)}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")}
	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
	for _, limit := range []int64{128, 0, 128} {
		cfg := &config.Config{}
		cfg.Codex.WebsocketMaxMessageBytes = limit
		if !service.applyConfigUpdateWithAuthSynthesis(context.Background(), cfg, false) {
			t.Fatal("reload failed")
		}
		executor, ok := manager.Executor("codex")
		if !ok {
			t.Fatal("missing Codex executor")
		}
		before := dials.Load()
		_, err := executor.ExecuteStream(ctx, auth, req, opts)
		var status interface{ StatusCode() int }
		wantStatus := http.StatusRequestEntityTooLarge
		if limit == 0 {
			wantStatus = http.StatusBadRequest
		}
		if !errors.As(err, &status) || status.StatusCode() != wantStatus {
			t.Fatalf("limit=%d: got %v, want %d", limit, err, wantStatus)
		}
		if (dials.Load() > before) != (limit == 0) {
			t.Fatalf("limit=%d: incorrect upstream dial behavior", limit)
		}
	}
}
