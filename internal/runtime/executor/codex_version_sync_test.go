package executor

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestCodexVersionSyncIdentityPolicy(t *testing.T) {
	for _, mode := range []string{"auto", "disabled", "passthrough", "api-key", "global", "account"} {
		t.Run(mode, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.CodexHeaderDefaults.VersionSync.Enabled = true
			auth := &cliproxyauth.Auth{ID: "test", Provider: "codex", Attributes: map[string]string{}}
			switch mode {
			case "disabled":
				cfg.CodexHeaderDefaults.VersionSync.Enabled = false
			case "passthrough":
				cfg.Codex.DisableCodexCloaking = true
			case "api-key":
				auth.Attributes["api_key"] = "synthetic"
			case "global":
				cfg.CodexHeaderDefaults.UserAgent = "codex_exec/1.2.3"
				cfg.CodexHeaderDefaults.Version = "1.2.3"
			case "account":
				auth.Attributes["header:User-Agent"] = "codex_exec/2.3.4"
				auth.Attributes["header:Version"] = "2.3.4"
				auth.Attributes["header:Originator"] = "pinned"
			}
			source := http.Header{"User-Agent": {"native/7.8.9"}, "Version": {"7.8.9"}, "Originator": {"native"}}
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", nil)
			applyCodexHeadersFromSources(req, auth, "", true, cfg, source)
			applyCodexSyncedIdentityVersion(req.Context(), req.Header, cfg, auth, source, "0.163.0")
			headers := req.Header
			ws := applyCodexWebsocketHeaders(context.Background(), http.Header{}, auth, "", cfg, true, source)
			applyCodexSyncedIdentityVersion(context.Background(), ws, cfg, auth, source, "0.163.0")
			for _, name := range []string{"User-Agent", "Version", "Originator"} {
				if ws.Get(name) != headers.Get(name) {
					t.Fatalf("HTTP/WS %s mismatch: %q != %q", name, headers.Get(name), ws.Get(name))
				}
			}
			switch mode {
			case "auto":
				v := "0.163.0"
				if headers.Get("Version") != v || !strings.HasPrefix(headers.Get("User-Agent"), "codex-tui/"+v+" ") || !strings.Contains(headers.Get("User-Agent"), "; "+v+")") || headers.Get("Originator") != "codex-tui" {
					t.Fatalf("inconsistent automatic identity: %v", headers)
				}
			case "disabled", "passthrough", "api-key":
				if headers.Get("Version") != "7.8.9" {
					t.Fatal("inactive sync changed client version")
				}
			case "global":
				if headers.Get("User-Agent") != "codex_exec/0.163.0" || headers.Get("Version") != "0.163.0" {
					t.Fatal("global version was not overridden")
				}
			case "account":
				if headers.Get("User-Agent") != "codex_exec/0.163.0" || headers.Get("Version") != "0.163.0" || headers.Get("Originator") != "pinned" {
					t.Fatal("account version override or originator preservation failed")
				}
			}
		})
	}
}
