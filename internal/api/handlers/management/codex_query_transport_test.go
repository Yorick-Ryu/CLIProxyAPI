package management

import (
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestCodexQueryCompatibilityGating(t *testing.T) {
	cfg := &config.Config{}
	cfg.Codex.HTTPTLSProfile = "linux"
	h := &Handler{cfg: cfg}
	auth := &coreauth.Auth{Provider: "codex"}
	req, _ := http.NewRequest("GET", "https://chatgpt.com/backend-api/wham/usage", nil)
	route := "/v8/management/requests/api-call"
	if !h.codexQueryCompatibility(route, auth, req) {
		t.Fatal("Linux query not enabled")
	}
	if h.codexQueryCompatibility("/v0/management/api-call", auth, req) {
		t.Fatal("legacy behavior changed")
	}
	cfg.Codex.HTTPTLSProfile = "chrome"
	cfg.Codex.WebsocketTLSProfile = "codex"
	if h.codexQueryCompatibility(route, auth, req) {
		t.Fatal("WS profile enabled HTTP compatibility")
	}
	cfg.Codex.HTTPTLSProfile = "linux"
	auth.Attributes = map[string]string{"api_key": "synthetic"}
	if h.codexQueryCompatibility(route, auth, req) {
		t.Fatal("API key affected")
	}
	auth.Attributes = nil
	auth.Provider = "claude"
	if h.codexQueryCompatibility(route, auth, req) {
		t.Fatal("other provider affected")
	}
	if h.codexQueryCompatibility(route, nil, req) {
		t.Fatal("anonymous query affected")
	}
}
