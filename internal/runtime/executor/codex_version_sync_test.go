package executor

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
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

func TestCodexAccountHistoryIdentityAcrossTransportsAndReauthentication(t *testing.T) {
	versions := []string{"0.161.0", "0.162.0", "0.162.1", "0.163.0", "0.164.0"}
	cfg := &config.Config{}
	cfg.CodexHeaderDefaults.AccountUserAgent.Enabled = true
	cfg.CodexHeaderDefaults.UserAgent = "configured/1"
	cfg.CodexHeaderDefaults.Version = "1.0.0"
	auth := &cliproxyauth.Auth{
		ID: "original-file.json", Provider: "codex",
		Metadata:   map[string]any{"account_id": "synthetic-account", "chatgpt_user_id": "synthetic-user"},
		Attributes: map[string]string{"header:User-Agent": "codex-tui/2.3.4 (Ubuntu 24.4.0; x86_64) kitty (codex-tui; 2.3.4)", "header:Version": "2.0.0", "header:Originator": "operator"},
	}
	source := http.Header{"User-Agent": {"downstream/3"}, "Version": {"3.0.0"}}
	var want http.Header
	for _, mode := range []string{"http", "sse", "websocket", "reauthenticated", "image", "alpha-search"} {
		t.Run(mode, func(t *testing.T) {
			account := auth.Clone()
			if mode == "reauthenticated" {
				account.ID = "renamed-file.json"
				account.Metadata["access_token"] = "new-synthetic-token"
			}
			request, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", nil)
			applyCodexHeadersFromSources(request, account, "", mode != "http", cfg, source)
			headers := request.Header
			if mode == "websocket" {
				headers = applyCodexWebsocketHeaders(context.Background(), http.Header{}, account, "", cfg, true, source)
			}
			if mode == "image" {
				cfg.Codex.HTTPTLSProfile = "linux"
				applyCodexDirectImageHeaders(request, account, "", true, cfg, source)
				applyCodexImageCompatibilityHeaders(request, account, cfg, nil, source)
			}
			if mode == "alpha-search" {
				cfg.Codex.HTTPTLSProfile = "linux"
				request.URL.Path = "/backend-api/codex/alpha/search"
				request.Method = http.MethodPost
				if err := NewCodexExecutor(cfg).PrepareRequest(request, account); err != nil {
					t.Fatal(err)
				}
				headers = request.Header
			}
			applyCodexAccountHistoryIdentity(request.Context(), headers, cfg, account, source, versions)
			identity := http.Header{"User-Agent": {headers.Get("User-Agent")}, "Version": {headers.Get("Version")}, "Originator": {headers.Get("Originator")}}
			version := identity.Get("Version")
			if !strings.HasPrefix(identity.Get("User-Agent"), "codex-tui/"+version+" (") || !strings.HasSuffix(identity.Get("User-Agent"), "(codex-tui; "+version+")") || identity.Get("Originator") != "operator" {
				t.Fatalf("inconsistent account identity: %v", identity)
			}
			if identity.Get("User-Agent") != strings.ReplaceAll(account.Attributes["header:User-Agent"], "2.3.4", version) {
				t.Fatal("automatic profile replaced the account's explicit system or terminal")
			}
			if want == nil {
				want = identity
			} else if !reflect.DeepEqual(identity, want) {
				t.Fatalf("account identity changed with transport or credentials: got %v, want %v", identity, want)
			}
		})
	}
}

func TestCodexAccountHistoryIdentityScopeAndColdStart(t *testing.T) {
	versions := []string{"0.161.0", "0.162.0", "0.162.1", "0.163.0", "0.164.0"}
	for _, mode := range []string{"disabled", "legacy", "passthrough", "account-passthrough", "api-key", "siwc", "nil-auth", "no-identity", "cold-start"} {
		t.Run(mode, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.CodexHeaderDefaults.AccountUserAgent.Enabled = true
			auth := &cliproxyauth.Auth{ID: "test", Provider: "codex", Attributes: map[string]string{}}
			history := versions
			switch mode {
			case "disabled":
				cfg.CodexHeaderDefaults.AccountUserAgent.Enabled = false
			case "legacy":
				cfg.CodexHeaderDefaults.AccountUserAgent.Enabled = false
				cfg.CodexHeaderDefaults.VersionSync.Enabled = true
			case "passthrough":
				cfg.Codex.DisableCodexCloaking = true
			case "account-passthrough":
				auth.Attributes[cliproxyauth.AttributeCodexDisableCloaking] = "true"
			case "api-key":
				auth.Attributes["api_key"] = "synthetic"
			case "siwc":
				auth.Metadata = map[string]any{"use_siwc": true}
			case "nil-auth":
				auth = nil
			case "no-identity":
				auth.ID = ""
			case "cold-start":
				history = nil
			}
			headers := http.Header{"User-Agent": {"unchanged/1"}, "Version": {"1"}, "Originator": {"unchanged"}}
			before := headers.Clone()
			applyCodexAccountHistoryIdentity(context.Background(), headers, cfg, auth, nil, history)
			if !reflect.DeepEqual(before, headers) {
				t.Fatalf("changed headers outside the active OAuth account history scope: %v", headers)
			}
		})
	}
}

func TestCodexAccountHistoryManualUAFollowsVersionSlot(t *testing.T) {
	old := []string{"0.161.0", "0.162.0", "0.162.1", "0.163.0", "0.164.0"}
	next := []string{"0.162.0", "0.162.1", "0.163.0", "0.164.0", "0.165.0"}
	manual := "codex-tui/9.8.7 (Arch Linux Rolling Release; x86_64) ghostty/1.3.1-arch2 (codex-tui; 9.8.7)"
	for _, tc := range []struct {
		name, rule, source, want string
	}{
		{"manual", manual, "", manual},
		{"unknown-client", "operator/2", "", "operator/2"},
		{"reference", "$User-Agent", manual, manual},
		{"missing-reference", "$X-Missing", "", ""},
		{"empty", "  ", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.CodexHeaderDefaults.AccountUserAgent.Enabled = true
			cfg.CodexHeaderDefaults.VersionSync.Enabled = true
			auth := &cliproxyauth.Auth{ID: "manual-account", Provider: "codex", Attributes: map[string]string{"header:User-Agent": tc.rule, "header:Version": "pinned"}}
			before := auth.Clone()
			source := http.Header{"User-Agent": {tc.source}}
			for _, history := range [][]string{old, next} {
				generated, version := helps.CodexAccountUserAgent(codexCredentialIdentitySource(auth), history)
				headers := http.Header{"User-Agent": {codexUserAgent}, "Version": {"pinned"}}
				applyCodexAccountHistoryIdentity(context.Background(), headers, cfg, auth, source, history)
				want := generated
				if tc.want != "" {
					want = strings.ReplaceAll(tc.want, "9.8.7", version)
				}
				if headers.Get("User-Agent") != want || headers.Get("Version") != version {
					t.Fatalf("manual profile or assigned version lost: %v", headers)
				}
				applyCodexSyncedIdentityVersion(context.Background(), headers, cfg, auth, source, "99.0.0")
				if headers.Get("User-Agent") != want || headers.Get("Version") != version {
					t.Fatal("global synchronization replaced the manual account profile")
				}
			}
			headers := http.Header{"User-Agent": {codexUserAgent}, "Version": {"pinned"}}
			applyCodexAccountHistoryIdentity(context.Background(), headers, cfg, auth, source, nil)
			if tc.want != "" && headers.Get("User-Agent") != tc.want || headers.Get("Version") != "pinned" {
				t.Fatal("cold startup replaced a manual UA or changed its configured version")
			}
			if !reflect.DeepEqual(auth, before) {
				t.Fatal("request-time version update mutated the stored credential")
			}
		})
	}
}

func TestCodexAccountHistoryCachedStartupFlowsIntoHeaders(t *testing.T) {
	cfg := &config.Config{AuthDir: t.TempDir()}
	cfg.ProxyURL = "http://127.0.0.1:1" // Discovery must never reach a real upstream in this test.
	cfg.Codex.HTTPTLSProfile = "linux"
	cfg.CodexHeaderDefaults.AccountUserAgent.Enabled = true
	if err := os.WriteFile(filepath.Join(cfg.AuthDir, ".codex-version-history"), []byte(`["0.161.0","0.162.0","0.162.1","0.163.0","0.164.0"]`), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	helps.StartCodexVersionUpdater(ctx, cfg)
	cancel()
	t.Cleanup(func() {
		resetCtx, resetCancel := context.WithCancel(context.Background())
		helps.StartCodexVersionUpdater(resetCtx, &config.Config{})
		resetCancel()
	})
	auth := &cliproxyauth.Auth{ID: "account.json", Provider: "codex", Metadata: map[string]any{"account_id": "synthetic-account"}}
	wantUA, wantVersion := helps.CodexAccountUserAgent(codexCredentialIdentitySource(auth), helps.CodexVersionHistory())
	if wantUA == "" {
		t.Fatal("cached history was not ready when startup returned")
	}
	for _, mode := range []string{"http", "sse", "websocket", "image", "alpha-search", "manual-http", "manual-sse", "manual-websocket", "manual-image", "manual-alpha-search", "manual-unknown-websocket"} {
		t.Run(mode, func(t *testing.T) {
			account := auth.Clone()
			expectedUA := wantUA
			transport := strings.TrimPrefix(mode, "manual-")
			if strings.HasPrefix(mode, "manual-") {
				custom := "codex-tui/9.8.7 (Ubuntu 24.4.0; x86_64) kitty (codex-tui; 9.8.7)"
				if transport == "unknown-websocket" {
					custom = "operator/2"
					transport = "websocket"
				}
				account.Attributes = map[string]string{"header:User-Agent": custom}
				expectedUA = strings.ReplaceAll(custom, "9.8.7", wantVersion)
			}
			request, _ := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", nil)
			applyCodexHeadersFromSources(request, account, "", transport == "sse", cfg, nil)
			switch transport {
			case "websocket":
				request.Header = applyCodexWebsocketHeaders(request.Context(), nil, account, "", cfg, true)
			case "image":
				applyCodexDirectImageHeaders(request, account, "", false, cfg)
				applyCodexImageCompatibilityHeaders(request, account, cfg, nil, nil)
			case "alpha-search":
				request.URL.Path = "/backend-api/codex/alpha/search"
				if err := NewCodexExecutor(cfg).PrepareRequest(request, account); err != nil {
					t.Fatal(err)
				}
			}
			if request.Header.Get("User-Agent") != expectedUA || request.Header.Get("Version") != wantVersion || request.Header.Get("Originator") != "codex-tui" {
				t.Fatalf("cached account profile did not reach %s request headers", mode)
			}
		})
	}
}

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
