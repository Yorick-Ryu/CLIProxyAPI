package executor

import (
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

// Both serial and duplex streams must preserve tenant/account isolation after
// the upstream request-preparation refactor.
func TestPrepareCodexWebsocketStreamPreservesAccountIdentity(t *testing.T) {
	exec := NewCodexWebsocketsExecutor(&config.Config{
		SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll},
		Codex:     config.CodexConfig{IdentityConvergence: true},
	})
	body := []byte(`{"model":"gpt-6-astra","instructions":"Reply OK","input":[],"prompt_cache_key":"client-session","client_metadata":{"session_id":"client-session","thread_id":"client-thread","x-codex-window-id":"client-thread:2","x-codex-installation-id":"client-device"}}`)
	prepare := func(caller, account string) *codexWebsocketPrepared {
		t.Helper()
		auth := &cliproxyauth.Auth{ID: account, Provider: "codex", Metadata: map[string]any{"account_id": account}}
		prepared, err := exec.prepareCodexWebsocketStream(codexAccountIdentityTestContext(caller), auth,
			cliproxyexecutor.Request{Model: "gpt-6-astra", Payload: body},
			cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("codex"), Headers: http.Header{
				"Session-Id":          {"client-session"},
				"Thread-Id":           {"client-thread"},
				"X-Client-Request-Id": {"client-thread"},
				"X-Codex-Window-Id":   {"client-thread:2"},
			}})
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
	if got := first.wsHeaders.Get("Session-Id"); got != session {
		t.Fatalf("CLI session header = %q, want %q", got, session)
	}
	for _, key := range []string{"session_id", "conversation_id"} {
		if got := headerValueCaseInsensitive(first.wsHeaders, key); got != "" {
			t.Fatalf("unexpected legacy %s = %q", key, got)
		}
	}
	for _, field := range []struct{ header, path, original string }{
		{"Thread-Id", "client_metadata.thread_id", "client-thread"},
		{"X-Codex-Window-Id", "client_metadata.x-codex-window-id", "client-thread:2"},
	} {
		got := first.wsHeaders.Get(field.header)
		if got == "" || got == field.original || got != gjson.GetBytes(first.upstreamBody, field.path).String() {
			t.Fatalf("%s must be scoped and match the request body, got %q", field.header, got)
		}
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
	if first.wsHeaders.Get("X-Client-Request-Id") != first.wsHeaders.Get("Thread-Id") {
		t.Fatal("client request and thread headers must retain their shared identity")
	}
	if first.wsHeaders.Get("X-Codex-Window-Id") != first.wsHeaders.Get("Thread-Id")+":2" {
		t.Fatal("window header must retain the mapped thread and context generation")
	}
	device := gjson.GetBytes(first.upstreamBody, "client_metadata.x-codex-installation-id").String()
	if device == "" || device == "client-device" {
		t.Fatal("body installation ID must still converge")
	}
	if headerValueCaseInsensitive(first.wsHeaders, "x-codex-installation-id") != "" {
		t.Fatal("installation convergence must not synthesize an extra header")
	}
}
