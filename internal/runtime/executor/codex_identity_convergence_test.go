package executor

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func TestCodexIdentityConvergenceUsesStableAccountDeviceAndSession(t *testing.T) {
	cfg := &config.Config{Codex: config.CodexConfig{
		IdentityConfuse:     true,
		IdentityConvergence: true,
	}}
	auth := &cliproxyauth.Auth{ID: "oauth-auth-a", Provider: "codex"}
	firstClientBody := []byte(`{"prompt_cache_key":"client-session-a","client_metadata":{"session_id":"client-session-a","x-codex-installation-id":"client-install-a","x-codex-window-id":"client-session-a:0","x-codex-turn-metadata":"{\"installation_id\":\"client-install-a\",\"session_id\":\"client-session-a\",\"thread_id\":\"client-session-a\",\"turn_id\":\"client-turn-a\",\"window_id\":\"client-session-a:0\"}"}}`)

	confusedBody, confusedState := applyCodexIdentityConfuseBody(cfg, auth, firstClientBody, firstClientBody)
	if confusedState.enabled || !bytes.Equal(confusedBody, firstClientBody) {
		t.Fatal("identity convergence must take precedence over identity confusion for Codex OAuth")
	}

	firstBody, first := applyCodexIdentityConvergenceBody(cfg, auth, firstClientBody, firstClientBody, http.Header{"Session-Id": []string{"client-session-a"}})
	if !first.enabled {
		t.Fatal("identity convergence was not enabled for Codex OAuth")
	}
	if got, want := gjson.GetBytes(firstBody, "client_metadata.x-codex-installation-id").String(), first.installationID; got != want {
		t.Fatalf("installation id = %q, want %q", got, want)
	}
	if version := uuid.MustParse(first.installationID).Version(); version != 4 {
		t.Fatalf("installation UUID version = %d, want 4", version)
	}
	if got, want := gjson.GetBytes(firstBody, "client_metadata.session_id").String(), first.sessionID; got != want {
		t.Fatalf("session id = %q, want %q", got, want)
	}
	if version := uuid.MustParse(first.sessionID).Version(); version != 4 {
		t.Fatalf("session UUID version = %d, want 4", version)
	}
	if got, want := gjson.GetBytes(firstBody, "client_metadata.thread_id").String(), first.threadID; got != want {
		t.Fatalf("thread id = %q, want %q", got, want)
	}
	if got, want := gjson.GetBytes(firstBody, "client_metadata.x-codex-window-id").String(), first.windowID; got != want {
		t.Fatalf("window id = %q, want %q", got, want)
	}
	if got, want := gjson.GetBytes(firstBody, "prompt_cache_key").String(), first.sessionID; got != want {
		t.Fatalf("prompt_cache_key = %q, want %q", got, want)
	}
	if !first.promptCacheKeyWasConverged {
		t.Fatal("expected prompt_cache_key to converge when it matches the client metadata session")
	}
	if _, err := uuid.Parse(gjson.GetBytes(firstBody, "client_metadata.turn_id").String()); err != nil {
		t.Fatalf("turn id is not a UUID: %v", err)
	}

	firstMetadata := gjson.GetBytes(firstBody, "client_metadata.x-codex-turn-metadata").String()
	for path, want := range map[string]string{
		"installation_id": first.installationID,
		"session_id":      first.sessionID,
		"thread_id":       first.threadID,
		"turn_id":         first.turnID,
		"window_id":       first.windowID,
	} {
		if got := gjson.Get(firstMetadata, path).String(); got != want {
			t.Fatalf("turn metadata %s = %q, want %q", path, got, want)
		}
	}
	if got := gjson.Get(firstMetadata, "turn_started_at_unix_ms").Int(); got != first.turnStartedAtUnixMs {
		t.Fatalf("turn metadata timestamp = %d, want %d", got, first.turnStartedAtUnixMs)
	}

	headers := http.Header{
		"Session-Id":            []string{"client-session-a"},
		"X-Codex-Turn-Metadata": []string{`{"installation_id":"client-install-a","session_id":"client-session-a","thread_id":"client-session-a","turn_id":"client-turn-a","window_id":"client-session-a:0"}`},
	}
	applyCodexIdentityConvergenceHeaders(headers, &first)
	if got, want := headers.Get("X-Codex-Installation-Id"), first.installationID; got != want {
		t.Fatalf("X-Codex-Installation-Id = %q, want %q", got, want)
	}
	if got, want := headers.Get("Session-Id"), first.sessionID; got != want {
		t.Fatalf("Session-Id = %q, want %q", got, want)
	}
	if got, want := headers.Get("X-Client-Request-Id"), first.threadID; got != want {
		t.Fatalf("X-Client-Request-Id = %q, want %q", got, want)
	}
	if got, want := headers.Get("Thread-Id"), first.threadID; got != want {
		t.Fatalf("Thread-Id = %q, want %q", got, want)
	}
	if got, want := headers.Get("X-Codex-Window-Id"), first.windowID; got != want {
		t.Fatalf("X-Codex-Window-Id = %q, want %q", got, want)
	}
	headerMetadata := headers.Get("X-Codex-Turn-Metadata")
	if got, want := gjson.Get(headerMetadata, "session_id").String(), first.sessionID; got != want {
		t.Fatalf("header metadata session id = %q, want %q", got, want)
	}

	secondClientBody := []byte(`{"prompt_cache_key":"client-session-b","client_metadata":{"session_id":"client-session-b","x-codex-installation-id":"client-install-b"}}`)
	_, second := applyCodexIdentityConvergenceBody(cfg, auth, secondClientBody, secondClientBody, http.Header{"Session-Id": []string{"client-session-b"}})
	if second.installationID != first.installationID || second.sessionID != first.sessionID {
		t.Fatal("one OAuth auth must retain one stable installation and session")
	}
	if second.threadID == first.threadID {
		t.Fatal("different client sessions must receive different stable upstream threads")
	}
	if second.turnID == first.turnID {
		t.Fatal("each request must receive a fresh turn id")
	}

	otherAuth := &cliproxyauth.Auth{ID: "oauth-auth-b", Provider: "codex"}
	_, other := applyCodexIdentityConvergenceBody(cfg, otherAuth, firstClientBody, firstClientBody, nil)
	if other.installationID == first.installationID || other.sessionID == first.sessionID {
		t.Fatal("different OAuth auths must not share converged device or session ids")
	}
}

func TestCodexIdentityConvergencePreservesAPIKeyAndWebsocketHeaderShape(t *testing.T) {
	cfg := &config.Config{Codex: config.CodexConfig{IdentityConvergence: true}}
	clientBody := []byte(`{"prompt_cache_key":"client-session","client_metadata":{"session_id":"client-session"}}`)
	apiKeyAuth := &cliproxyauth.Auth{
		ID:       "api-key-auth",
		Provider: "codex",
		Attributes: map[string]string{
			"api_key": "test-key",
		},
	}
	untouched, state := applyCodexIdentityConvergenceBody(cfg, apiKeyAuth, clientBody, clientBody, nil)
	if state.enabled || !bytes.Equal(untouched, clientBody) {
		t.Fatal("identity convergence must not change Codex API-key requests")
	}

	oauthAuth := &cliproxyauth.Auth{ID: "oauth-auth", Provider: "codex"}
	_, oauthState := applyCodexIdentityConvergenceBody(cfg, oauthAuth, clientBody, clientBody, nil)
	headers := http.Header{"session_id": []string{"client-session"}}
	applyCodexIdentityConvergenceHeaders(headers, &oauthState)
	if got, want := headers["session_id"], []string{oauthState.sessionID}; len(got) != 1 || got[0] != want[0] {
		t.Fatalf("websocket session_id = %#v, want %#v", got, want)
	}
	if got := headers.Get("Session-Id"); got != "" {
		t.Fatalf("Session-Id = %q, want only websocket session_id", got)
	}
}

func TestCodexIdentityConvergenceAccountModeOverridesGlobalDefault(t *testing.T) {
	clientBody := []byte(`{"prompt_cache_key":"client-session","client_metadata":{"session_id":"client-session","x-codex-installation-id":"client-install","thread_id":"client-thread"}}`)
	cfg := &config.Config{Codex: config.CodexConfig{IdentityConvergence: true}}

	offAuth := &cliproxyauth.Auth{
		ID:       "oauth-off",
		Provider: "codex",
		Metadata: map[string]any{codexIdentityConvergenceModeKey: "off"},
	}
	offBody, offState := applyCodexIdentityConvergenceBody(cfg, offAuth, clientBody, clientBody, nil)
	if offState.enabled || !bytes.Equal(offBody, clientBody) {
		t.Fatal("account-level off must override the enabled global default")
	}

	accountAuth := &cliproxyauth.Auth{
		ID:       "oauth-account-session",
		Provider: "codex",
		Metadata: map[string]any{codexIdentityConvergenceModeKey: "session"},
	}
	_, accountState := applyCodexIdentityConvergenceBody(&config.Config{}, accountAuth, clientBody, clientBody, nil)
	if !accountState.enabled || accountState.mode != codexIdentityConvergenceSession {
		t.Fatalf("account-level session mode = %#v, want enabled session", accountState)
	}
}

func TestCodexIdentityConvergenceSupportsDeviceAndFullAccountModes(t *testing.T) {
	clientBody := []byte(`{"prompt_cache_key":"client-session","client_metadata":{"session_id":"client-session","x-codex-installation-id":"client-install","thread_id":"client-thread"}}`)

	deviceAuth := &cliproxyauth.Auth{
		ID:       "oauth-device",
		Provider: "codex",
		Metadata: map[string]any{codexIdentityConvergenceModeKey: "device"},
	}
	deviceBody, deviceState := applyCodexIdentityConvergenceBody(&config.Config{}, deviceAuth, clientBody, clientBody, nil)
	if deviceState.mode != codexIdentityConvergenceDevice {
		t.Fatalf("device mode = %q", deviceState.mode)
	}
	if got := gjson.GetBytes(deviceBody, "client_metadata.x-codex-installation-id").String(); got != deviceState.installationID {
		t.Fatalf("device installation id = %q, want %q", got, deviceState.installationID)
	}
	if got := gjson.GetBytes(deviceBody, "client_metadata.session_id").String(); got != "client-session" {
		t.Fatalf("device mode changed session id to %q", got)
	}
	if got := gjson.GetBytes(deviceBody, "prompt_cache_key").String(); got != "client-session" {
		t.Fatalf("device mode changed prompt cache key to %q", got)
	}

	fullAuth := &cliproxyauth.Auth{
		ID:       "oauth-full",
		Provider: "codex",
		Metadata: map[string]any{codexIdentityConvergenceModeKey: "full"},
	}
	_, first := applyCodexIdentityConvergenceBody(&config.Config{}, fullAuth, clientBody, clientBody, nil)
	secondBody := []byte(`{"prompt_cache_key":"another-client-session","client_metadata":{"session_id":"another-client-session"}}`)
	_, second := applyCodexIdentityConvergenceBody(&config.Config{}, fullAuth, secondBody, secondBody, nil)
	if first.mode != codexIdentityConvergenceFull || second.mode != codexIdentityConvergenceFull {
		t.Fatalf("full mode states = %q, %q", first.mode, second.mode)
	}
	if first.threadID != second.threadID || first.windowID != second.windowID {
		t.Fatal("full mode must keep one stable thread and window per account")
	}
}
