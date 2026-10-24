package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestCodexImageLinuxAllExecutionPaths(t *testing.T) {
	for _, linux := range []bool{false, true} {
		for _, direct := range []bool{false, true} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("linux=%v/direct=%v/stream=%v", linux, direct, stream), func(t *testing.T) {
					var got http.Header
					var path string
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						got = r.Header.Clone()
						path = r.URL.Path
						_, _ = io.Copy(io.Discard, r.Body)
						if !direct {
							w.Header().Set("Content-Type", "text/event-stream")
							fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"image_generation_call\",\"result\":\"AA==\"}]}}\n\n")
						} else if stream {
							w.Header().Set("Content-Type", "text/event-stream")
							fmt.Fprint(w, "data: {\"type\":\"image_generation.completed\",\"b64_json\":\"AA==\"}\n\n")
						} else {
							w.Header().Set("Content-Type", "application/json")
							fmt.Fprint(w, `{"data":[{"b64_json":"AA=="}]}`)
						}
					}))
					defer server.Close()
					cfg := &config.Config{}
					if linux {
						cfg.Codex.HTTPTLSProfile = "linux"
					}
					auth := &cliproxyauth.Auth{ID: "image-wire", Provider: "codex", Attributes: map[string]string{"base_url": server.URL}, Metadata: map[string]any{"access_token": "synthetic", "account_id": "synthetic"}}
					model := "image-compat-fallback"
					if direct {
						model = "gpt-image-2"
					}
					request := cliproxyexecutor.Request{Model: model, Payload: []byte(fmt.Sprintf(`{"model":%q,"prompt":"test"}`, model))}
					opts := codexOpenAIImageTestOptions(codexImagesGenerationsPath, stream)
					e := NewCodexExecutor(cfg)
					if stream {
						result, err := e.ExecuteStream(context.Background(), auth, request, opts)
						if err != nil {
							t.Fatal(err)
						}
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								t.Fatal(chunk.Err)
							}
						}
					} else {
						if _, err := e.Execute(context.Background(), auth, request, opts); err != nil {
							t.Fatal(err)
						}
					}
					wantPath := "/responses"
					if direct {
						wantPath = "/images/generations"
					}
					if path != wantPath {
						t.Fatalf("path=%s", path)
					}
					wantEncoding := "gzip"
					if linux {
						wantEncoding = ""
					}
					if got.Get("Accept-Encoding") != wantEncoding {
						t.Fatalf("automatic encoding=%q", got.Get("Accept-Encoding"))
					}
					if linux && direct {
						if got.Get("Accept") != "*/*" || got.Get("Version") != "0.162.0" {
							t.Fatalf("image defaults: accept=%q version=%q", got.Get("Accept"), got.Get("Version"))
						}
						id, err := uuid.Parse(got.Get("X-Codex-Image-Turn-Id"))
						if err != nil || id.Version() != 7 {
							t.Fatal("missing UUIDv7 image turn")
						}
					}
					if !direct && got.Get("X-Codex-Image-Turn-Id") != "" {
						t.Fatal("image header leaked to Responses")
					}
				})
			}
		}
	}
}

func TestCodexImageHeadersIdentityAndOverrides(t *testing.T) {
	cfg := &config.Config{}
	cfg.Codex.HTTPTLSProfile = "linux"
	auth := &cliproxyauth.Auth{ID: "a", Provider: "codex", Metadata: map[string]any{"account_id": "a"}}
	const turn = "019a0992-1500-7abc-8def-0123456789ab"
	source := http.Header{"Version": {"0.100.0"}, "X-Codex-Image-Turn-Id": {turn}, "Accept": {"custom/client"}}
	state := codexIdentityConfuseState{accountIdentity: resolveCodexAccountIdentityState(auth)}
	req, _ := http.NewRequest("POST", "https://chatgpt.com/backend-api/codex/images/edits", strings.NewReader("{}"))
	applyCodexDirectImageHeaders(req, auth, "synthetic", false, cfg, source)
	applyCodexImageCompatibilityHeaders(req, auth, cfg, &state, source)
	mapped := codexAccountIdentityUUID(state.accountIdentity, "turn", turn)
	if req.Header.Get("X-Codex-Image-Turn-Id") != mapped || mapped == turn {
		t.Fatal("image turn not account scoped")
	}
	if req.Header.Get("Version") != "0.162.0" || req.Header.Get("Accept") != "custom/client" {
		t.Fatal("identity version or client accept lost")
	}
	req.Header.Set("X-Codex-Turn-Metadata", `{"turn_id":"mapped-metadata-turn"}`)
	applyCodexImageCompatibilityHeaders(req, auth, cfg, &state, source)
	if req.Header.Get("X-Codex-Image-Turn-Id") != "mapped-metadata-turn" {
		t.Fatal("metadata turn linkage lost")
	}
	auth.Attributes = map[string]string{"header:X-Codex-Image-Turn-Id": "admin-turn", "header:Accept": "admin/type", "header:User-Agent": "codex-tui/1.2.3 custom", "header:Originator": "admin", "header:Version": "admin-version"}
	applyCodexDirectImageHeaders(req, auth, "synthetic", false, cfg, source)
	applyCodexImageCompatibilityHeaders(req, auth, cfg, &state, source)
	if req.Header.Get("X-Codex-Image-Turn-Id") != "admin-turn" || req.Header.Get("Accept") != "admin/type" || req.Header.Get("Version") != "admin-version" || req.Header.Get("User-Agent") != "codex-tui/1.2.3 custom" || req.Header.Get("Originator") != "admin" {
		t.Fatal("admin overrides lost")
	}
}

func TestCodexImageCompatibilityCredentialGating(t *testing.T) {
	for _, kind := range []string{"disabled", "api-key", "siwc", "nil"} {
		t.Run(kind, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Codex.HTTPTLSProfile = "linux"
			auth := &cliproxyauth.Auth{ID: "test", Provider: "codex", Attributes: map[string]string{}, Metadata: map[string]any{}}
			switch kind {
			case "disabled":
				cfg.Codex.HTTPTLSProfile = "chrome"
			case "api-key":
				auth.Attributes["api_key"] = "synthetic"
			case "siwc":
				auth.Metadata["use_siwc"] = true
			case "nil":
				auth = nil
			}
			req, _ := http.NewRequest("POST", "https://chatgpt.com/backend-api/codex/images/generations", nil)
			req.Header.Set("Accept", "original")
			req.Header.Set("Version", "original")
			applyCodexImageCompatibilityHeaders(req, auth, cfg, nil, nil)
			if req.Header.Get("Accept") != "original" || req.Header.Get("Version") != "original" || req.Header.Get("X-Codex-Image-Turn-Id") != "" {
				t.Fatal("ineligible image request changed")
			}
		})
	}
}
