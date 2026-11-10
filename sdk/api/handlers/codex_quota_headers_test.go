package handlers

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
)

func TestCodexQuotaHeaderScope(t *testing.T) {
	for _, name := range []string{
		"X-Codex-Primary-Used-Percent", "x-codex-secondary-reset-at",
		"x-codex-bengalfox-primary-window-minutes", "x-codex-additional-gpt5.3-primary-reset-after-seconds",
		"x-codex-credits-balance", "x-codex-credits-unlimited", "x-codex-credits-has-credits",
		"x-codex-active-limit", "x-codex-plan-type", "x-codex-rate-limit-reached-type",
	} {
		if !IsCodexQuotaHeader(name) {
			t.Errorf("quota header not recognized: %s", name)
		}
	}
	for _, name := range []string{
		"Retry-After", "x-ratelimit-remaining-requests", "x-models-etag",
		"x-codex-turn-state", "x-codex-safety-buffering-enabled", "x-request-id",
		"x-codex-primary-used-percent-extra", "x-other-primary-used-percent",
	} {
		if IsCodexQuotaHeader(name) {
			t.Errorf("unrelated header recognized: %s", name)
		}
	}
}

func TestCodexQuotaErrorResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, direct := range []bool{false, true} {
		for _, enabled := range []bool{false, true} {
			headers := http.Header{"X-Codex-Credits-Balance": {"20"}, "Retry-After": {"30"}, "X-Request-Id": {"keep"}}
			msg := &interfaces.ErrorMessage{
				StatusCode: http.StatusTooManyRequests, Error: errors.New("quota unavailable"),
				Addon: headers, Headers: headers, DirectResponse: direct,
				Body: []byte(`{"error":{"message":"quota unavailable"}}`),
			}
			cfg := &config.SDKConfig{PassthroughHeaders: true}
			cfg.Client.Codex.StripQuota = enabled
			r := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(r)
			NewBaseAPIHandlers(cfg, nil).WriteErrorResponse(c, msg)
			if r.Code != 429 || r.Header().Get("Retry-After") != "30" || r.Header().Get("X-Request-Id") != "keep" {
				t.Fatalf("error semantics changed: %d %v", r.Code, r.Header())
			}
			if (r.Header().Get("X-Codex-Credits-Balance") == "") != enabled {
				t.Fatalf("unexpected quota header with enabled=%t", enabled)
			}
			if headers.Get("X-Codex-Credits-Balance") != "20" {
				t.Fatal("mutated upstream quota data")
			}
		}
	}
}
