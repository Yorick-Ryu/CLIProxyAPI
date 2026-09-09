package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func codexAccountIdentityTestContext(apiKey string) context.Context {
	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	ginContext.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ginContext.Set("userApiKey", apiKey)
	return context.WithValue(context.Background(), "gin", ginContext)
}

func TestCodexAccountIdentityScopesByCallerCredentialAndRawIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"prompt_cache_key":"client-session-a","client_metadata":{"session_id":"client-session-a","thread_id":"client-thread-a","x-codex-installation-id":"client-install-a","x-codex-window-id":"client-window-a","x-codex-turn-metadata":"{\"installation_id\":\"client-install-a\",\"session_id\":\"client-session-a\",\"thread_id\":\"client-thread-a\",\"turn_id\":\"client-turn-a\",\"window_id\":\"client-window-a\"}"}}`)
	auth := &cliproxyauth.Auth{
		ID:       "local-auth-a",
		Provider: "codex",
		Metadata: map[string]any{"account_id": "upstream-account-a"},
	}

	firstBody, first := applyCodexAccountIdentityBody(codexAccountIdentityTestContext("caller-key-a"), auth, body)
	secondBody, second := applyCodexAccountIdentityBody(codexAccountIdentityTestContext("caller-key-a"), auth, body)
	if !first.enabled || !second.enabled || string(firstBody) != string(secondBody) {
		t.Fatal("the same caller, credential, and raw identity must map deterministically")
	}
	mappedSession := gjson.GetBytes(firstBody, "client_metadata.session_id").String()
	if mappedSession == "" || mappedSession == "client-session-a" {
		t.Fatalf("mapped session = %q, want a non-empty anonymous value", mappedSession)
	}
	if got := gjson.GetBytes(firstBody, "prompt_cache_key").String(); got != mappedSession {
		t.Fatalf("mapped prompt_cache_key = %q, want mapped session %q", got, mappedSession)
	}
	if got := gjson.Get(gjson.GetBytes(firstBody, "client_metadata.x-codex-turn-metadata").String(), "session_id").String(); got != mappedSession {
		t.Fatalf("embedded session = %q, want %q", got, mappedSession)
	}

	headers := http.Header{
		"Session-Id":              []string{"client-session-a"},
		"Thread-Id":               []string{"client-thread-a"},
		"X-Codex-Installation-Id": []string{"client-install-a"},
	}
	applyCodexAccountIdentityHeaders(headers, &first)
	if got := headers.Get("Session-Id"); got != mappedSession {
		t.Fatalf("mapped Session-Id = %q, want body session %q", got, mappedSession)
	}
	if got, want := headers.Get("Thread-Id"), gjson.GetBytes(firstBody, "client_metadata.thread_id").String(); got != want {
		t.Fatalf("mapped Thread-Id = %q, want body thread %q", got, want)
	}

	differentCallerBody, _ := applyCodexAccountIdentityBody(codexAccountIdentityTestContext("caller-key-b"), auth, body)
	if got := gjson.GetBytes(differentCallerBody, "client_metadata.session_id").String(); got == mappedSession {
		t.Fatal("different downstream API keys must not share an upstream session identity")
	}
	differentAuth := &cliproxyauth.Auth{
		ID:       "local-auth-b",
		Provider: "codex",
		Metadata: map[string]any{"account_id": "upstream-account-b"},
	}
	differentAccountBody, _ := applyCodexAccountIdentityBody(codexAccountIdentityTestContext("caller-key-a"), differentAuth, body)
	if got := gjson.GetBytes(differentAccountBody, "client_metadata.session_id").String(); got == mappedSession {
		t.Fatal("different OAuth accounts must not share an upstream session identity")
	}
	differentSession := []byte(`{"prompt_cache_key":"client-session-b","client_metadata":{"session_id":"client-session-b","thread_id":"client-thread-b"}}`)
	differentSessionBody, _ := applyCodexAccountIdentityBody(codexAccountIdentityTestContext("caller-key-a"), auth, differentSession)
	if got := gjson.GetBytes(differentSessionBody, "client_metadata.session_id").String(); got == mappedSession {
		t.Fatal("different client sessions must remain distinct after remapping")
	}
}

func TestCodexAccountIdentityUsesStableOAuthAccountAndComposesWithDeviceMode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := codexAccountIdentityTestContext("caller-key")
	body := []byte(`{"prompt_cache_key":"client-session","client_metadata":{"session_id":"client-session","thread_id":"client-thread","x-codex-installation-id":"client-install"}}`)
	firstAuth := &cliproxyauth.Auth{ID: "row-a", Provider: "codex", Metadata: map[string]any{"account_id": "shared-upstream-account"}}
	secondAuth := &cliproxyauth.Auth{ID: "row-b", Provider: "codex", Metadata: map[string]any{"account_id": "shared-upstream-account"}}

	firstScoped, firstIdentity := applyCodexAccountIdentityBody(ctx, firstAuth, body)
	secondScoped, _ := applyCodexAccountIdentityBody(ctx, secondAuth, body)
	if string(firstScoped) != string(secondScoped) {
		t.Fatal("two local auth rows for the same OAuth account must share an identity namespace")
	}

	cfg := &config.Config{Codex: config.CodexConfig{IdentityConvergence: true}}
	firstFinal, firstConvergence := applyCodexIdentityConvergenceBody(cfg, firstAuth, firstScoped, firstScoped, nil)
	secondFinal, secondConvergence := applyCodexIdentityConvergenceBody(cfg, secondAuth, secondScoped, secondScoped, nil)
	if firstConvergence.mode != codexIdentityConvergenceDevice || secondConvergence.mode != codexIdentityConvergenceDevice {
		t.Fatal("global convergence must compose with account scoping in device mode")
	}
	if firstConvergence.installationID != secondConvergence.installationID {
		t.Fatal("the same OAuth account must retain one converged device across local auth rows")
	}
	if got, want := gjson.GetBytes(firstFinal, "client_metadata.session_id").String(), gjson.GetBytes(firstScoped, "client_metadata.session_id").String(); got != want {
		t.Fatalf("device mode changed scoped session from %q to %q", want, got)
	}
	if got, want := gjson.GetBytes(secondFinal, "prompt_cache_key").String(), gjson.GetBytes(secondScoped, "prompt_cache_key").String(); got != want {
		t.Fatalf("device mode changed scoped prompt cache key from %q to %q", want, got)
	}

	headers := http.Header{"Session-Id": []string{"client-session"}, "X-Codex-Installation-Id": []string{"client-install"}}
	applyCodexAccountIdentityHeaders(headers, &firstIdentity)
	applyCodexIdentityConvergenceHeaders(headers, &firstConvergence)
	if got, want := headers.Get("Session-Id"), gjson.GetBytes(firstFinal, "client_metadata.session_id").String(); got != want {
		t.Fatalf("composed Session-Id = %q, want %q", got, want)
	}
	if got := headers.Get("X-Codex-Installation-Id"); got != firstConvergence.installationID {
		t.Fatalf("composed installation header = %q, want %q", got, firstConvergence.installationID)
	}
}

func TestCodexAccountIdentityLeavesAPIKeyAndMissingFieldsUntouched(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"gpt-5.6"}`)
	oauth := &cliproxyauth.Auth{ID: "oauth", Provider: "codex", Metadata: map[string]any{"account_id": "account"}}
	oauthBody, state := applyCodexAccountIdentityBody(codexAccountIdentityTestContext("caller"), oauth, body)
	if !state.enabled || string(oauthBody) != string(body) {
		t.Fatal("OAuth account scoping must not synthesize missing identity fields")
	}
	apiKeyAuth := &cliproxyauth.Auth{ID: "api-key", Provider: "codex", Attributes: map[string]string{"api_key": "upstream-key"}}
	apiKeyBody, apiKeyState := applyCodexAccountIdentityBody(codexAccountIdentityTestContext("caller"), apiKeyAuth, body)
	if apiKeyState.enabled || string(apiKeyBody) != string(body) {
		t.Fatal("Codex API-key upstreams must remain untouched")
	}
}
