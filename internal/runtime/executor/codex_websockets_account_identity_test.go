package executor

import (
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

// Both serial and duplex streams must preserve tenant/account isolation after
// the upstream request-preparation refactor.
func TestPrepareCodexWebsocketStreamPreservesAccountIdentity(t *testing.T) {
	exec := NewCodexWebsocketsExecutor(&config.Config{
		SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll},
		Codex:     config.CodexConfig{IdentityConvergence: true},
	})
	body := []byte(`{"model":"gpt-6-astra","instructions":"Reply OK","input":[],"prompt_cache_key":"client-session","client_metadata":{"session_id":"client-session","x-codex-installation-id":"client-device"}}`)
	prepare := func(caller, account string) *codexWebsocketPrepared {
		t.Helper()
		auth := &cliproxyauth.Auth{ID: account, Provider: "codex", Metadata: map[string]any{"account_id": account}}
		prepared, err := exec.prepareCodexWebsocketStream(codexAccountIdentityTestContext(caller), auth,
			cliproxyexecutor.Request{Model: "gpt-6-astra", Payload: body},
			cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("codex"), Headers: http.Header{"Session_id": []string{"client-session"}}})
		if err != nil {
			t.Fatal(err)
		}
		return prepared
	}
	first := prepare("caller-a", "account-a")
	session := gjson.GetBytes(first.upstreamBody, "prompt_cache_key").String()
	if session == "" || session == "client-session" {
		t.Fatal("upstream session must be scoped")
	}
	if got := codexSessionHeaderValue(first.wsHeaders); got != session {
		t.Fatalf("header session %q differs from body session %q", got, session)
	}
	if got := gjson.GetBytes(first.clientBody, "prompt_cache_key").String(); got != "client-session" {
		t.Fatal("client identity must remain available for response restoration")
	}
	if got := gjson.GetBytes(prepare("caller-a", "account-a").upstreamBody, "prompt_cache_key").String(); got != session {
		t.Fatal("same caller and account must preserve the session across turns")
	}
	for _, other := range []*codexWebsocketPrepared{prepare("caller-b", "account-a"), prepare("caller-a", "account-b")} {
		if gjson.GetBytes(other.upstreamBody, "prompt_cache_key").String() == session {
			t.Fatal("session leaked across caller or account boundary")
		}
	}
	device := headerValueCaseInsensitive(first.wsHeaders, "x-codex-installation-id")
	if device == "" || device == "client-device" || device != gjson.GetBytes(first.upstreamBody, "client_metadata.x-codex-installation-id").String() {
		t.Fatal("converged device must match between headers and body")
	}
}
