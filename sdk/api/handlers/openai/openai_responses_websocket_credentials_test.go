package openai

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestResponsesWebsocketAvailabilityUsesRequestCredentials(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name                                                  string
		native, expiredOuter, expiredSIWC, rejectedSIWC, want bool
	}{
		{name: "native uses valid outer despite expired SIWC", native: true, expiredSIWC: true, want: true},
		{name: "native uses valid outer despite rejected SIWC", native: true, rejectedSIWC: true, want: true},
		{name: "native rejects expired outer despite valid SIWC", native: true, expiredOuter: true},
		{name: "non-native rejects expired SIWC", expiredSIWC: true},
		{name: "non-native uses valid SIWC despite expired outer", expiredOuter: true, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expiration := func(expired bool) string {
				if expired {
					return now.Add(-time.Hour).Format(time.RFC3339)
				}
				return now.Add(time.Hour).Format(time.RFC3339)
			}
			auth := &coreauth.Auth{
				ID: "ws-request-credentials", Provider: "codex", Status: coreauth.StatusActive,
				Attributes: map[string]string{"websockets": "true"},
				Metadata: map[string]any{
					"type": "codex", "use_siwc": true, "access_token": "test-outer", "expired": expiration(tc.expiredOuter),
					"siwc": map[string]any{"access_token": "test-inner", "expired": expiration(tc.expiredSIWC)},
				},
			}
			if tc.rejectedSIWC {
				auth.RejectedSIWCAccessToken = "test-inner"
			}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("GET", "/v1/responses", nil)
			if tc.native {
				c.Request.Header.Set("User-Agent", "codex_cli_rs/1.0.0")
			} else {
				c.Request.Header.Set("User-Agent", "test-client")
			}
			ctx := context.WithValue(c.Request.Context(), "gin", c)
			manager := coreauth.NewManager(nil, nil, nil)
			manager.RegisterExecutor(&websocketProviderCaptureExecutor{provider: "codex"})
			if _, err := manager.Register(context.Background(), auth); err != nil {
				t.Fatal(err)
			}
			const model = "ws-request-credentials-model"
			registry.GetGlobalRegistry().RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: model}})
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
			h := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager))
			if got := h.responsesWebsocketUsesUpstreamWebsocketPassthrough(ctx, model); got != tc.want {
				t.Errorf("passthrough = %v, want %v", got, tc.want)
			}
			if got := responsesWebsocketPinnedAuthMatchesModel(ctx, auth, model, model, false); got != tc.want {
				t.Errorf("pinned match = %v, want %v", got, tc.want)
			}
			if got := h.websocketUpstreamSupportsIncrementalInputForModel(ctx, model); got != tc.want {
				t.Errorf("incremental support = %v, want %v", got, tc.want)
			}
			if got := h.websocketUpstreamSupportsCompactionReplayForModel(ctx, model); got != tc.want {
				t.Errorf("compaction support = %v, want %v", got, tc.want)
			}
		})
	}
}
