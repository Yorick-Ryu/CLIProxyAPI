package executor

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestCodexDefaultLinuxTUIIdentity(t *testing.T) {
	cfg := &config.Config{}
	auth := &cliproxyauth.Auth{ID: "linux-default", Provider: "codex"}
	request, _ := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", nil)
	applyCodexHeadersFromSources(request, auth, "", true, cfg, nil)
	ws := applyCodexWebsocketHeaders(context.Background(), http.Header{}, auth, "", cfg, true, nil)
	for _, headers := range []http.Header{request.Header, ws} {
		if !strings.Contains(headers.Get("User-Agent"), "(Ubuntu 24.4.0; x86_64) xterm-256color") || headers.Get("Originator") != "codex-tui" {
			t.Fatalf("default identity %q / %q", headers.Get("User-Agent"), headers.Get("Originator"))
		}
	}
	cfg.CodexHeaderDefaults.UserAgent = "custom-client/1.0"
	applyCodexHeadersFromSources(request, auth, "", true, cfg, nil)
	if request.Header.Get("User-Agent") != "custom-client/1.0" {
		t.Fatal("explicit UA overridden by Linux default")
	}
}

func TestCodexPersistedAccountHeadersAcrossTransports(t *testing.T) {
	cfg := &config.Config{}
	cfg.CodexHeaderDefaults.UserAgent = "global/1"
	cfg.CodexHeaderDefaults.Version = "0.999.0"
	auth := &cliproxyauth.Auth{ID: "account", Provider: "codex", Attributes: map[string]string{
		"header:User-Agent": "codex-tui/0.163.0 (Debian 12.0.0; x86_64) kitty (codex-tui; 0.163.0)",
		"header:Version":    "0.163.0", "header:Originator": "codex-tui",
	}}
	for _, client := range []http.Header{nil, {"User-Agent": {"caller/2"}, "Version": {"0.888.0"}}} {
		request, _ := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", nil)
		applyCodexHeadersFromSources(request, auth, "", true, cfg, client)
		ws := applyCodexWebsocketHeaders(context.Background(), http.Header{}, auth, "", cfg, true, client)
		for _, headers := range []http.Header{request.Header, ws} {
			for _, name := range []string{"User-Agent", "Version", "Originator"} {
				if got, want := headers.Get(name), auth.Attributes["header:"+name]; got != want {
					t.Fatalf("%s = %q, want %q", name, got, want)
				}
			}
		}
	}
}
