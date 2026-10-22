package executor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

type codexAlphaSearchRoundTripFunc func(*http.Request) (*http.Response, error)

func (f codexAlphaSearchRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestCodexAlphaSearchNativeHeadersAndScopedIdentity(t *testing.T) {
	const thread = "019a0992-1400-7abc-8def-0123456789ab"
	const turn = "019a0992-1470-7abc-8def-0123456789ab"
	const body = `{"id":"` + thread + `","model":"gpt-test","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"keep me"}],"internal_chat_message_metadata_passthrough":{"turn_id":"` + turn + `"}}],"commands":{"search_query":[{"q":"test"}]},"extension":{"keep":true}}`
	metadata := `{"session_id":"` + thread + `","thread_id":"` + thread + `","turn_id":"` + turn + `","turn_started_at_unix_ms":1234567890123,"extension":"测试"}`
	cfg := &config.Config{}
	cfg.Codex.HTTPTLSProfile = "linux"
	e := NewCodexExecutor(cfg)
	seen := map[string]bool{}
	for _, account := range []string{"a", "b"} {
		for _, caller := range []string{"one", "two"} {
			ctx := contextWithGinHeaders(map[string]string{"User-Agent": "codex-tui/0.163.0 native", "Version": "0.163.0", "Originator": "codex-tui", "X-Codex-Turn-Metadata": metadata})
			// The caller identity is part of the same namespace used by Responses.
			g := ctx.Value("gin").(interface{ Set(string, any) })
			g.Set("userApiKey", caller)
			auth := &cliproxyauth.Auth{ID: account, Provider: "codex", Metadata: map[string]any{"access_token": "synthetic", "account_id": account}}
			req, _ := http.NewRequestWithContext(ctx, "POST", "https://chatgpt.com/backend-api/codex/alpha/search", strings.NewReader(body))
			req.Header.Set("Originator", "codex_cli_rs")
			req.Header.Set("Accept", "application/json")
			if err := e.PrepareRequest(req, auth); err != nil {
				t.Fatal(err)
			}
			firstHeaders := req.Header.Clone()
			reader, _ := req.GetBody()
			firstBody, _ := io.ReadAll(reader)
			_ = reader.Close()
			if req.Header.Get("Originator") != "codex-tui" || req.Header.Get("Accept") != "*/*" {
				t.Fatal("search defaults not applied")
			}
			if req.Header.Get("Version") != "0.162.0" || req.Header.Get("User-Agent") != codexUserAgent {
				t.Fatal("fixed identity version is inconsistent")
			}
			m := gjson.Parse(req.Header.Get("X-Codex-Turn-Metadata"))
			mappedID := gjson.GetBytes(firstBody, "id").String()
			if mappedID == thread || m.Get("session_id").String() != mappedID || m.Get("thread_id").String() != mappedID {
				t.Fatal("search id and metadata lost their association")
			}
			expected := codexAccountIdentityUUID(resolveCodexAccountIdentityState(ctx, auth), "thread", thread)
			if mappedID != expected || seen[mappedID] {
				t.Fatal("caller/account namespace differs from Responses or was reused")
			}
			seen[mappedID] = true
			u, err := uuid.Parse(mappedID)
			if err != nil || u.Version() != 7 || u.String()[:13] != thread[:13] {
				t.Fatal("UUIDv7 timestamp changed")
			}
			if m.Get("turn_id").String() != gjson.GetBytes(firstBody, "input.0.internal_chat_message_metadata_passthrough.turn_id").String() {
				t.Fatal("history turn link changed")
			}
			if m.Get("turn_started_at_unix_ms").Int() != 1234567890123 || m.Get("extension").String() != "测试" || !gjson.GetBytes(firstBody, "extension.keep").Bool() || gjson.GetBytes(firstBody, "commands.search_query.0.q").String() != "test" {
				t.Fatal("business data changed")
			}
			for _, header := range []string{"Session-Id", "Thread-Id", "X-Codex-Window-Id", "X-Client-Request-Id"} {
				if req.Header.Get(header) != "" {
					t.Fatal("Responses-only header injected")
				}
			}
			if err := e.PrepareRequest(req, auth); err != nil {
				t.Fatal(err)
			}
			reader, _ = req.GetBody()
			secondBody, _ := io.ReadAll(reader)
			_ = reader.Close()
			if string(firstBody) != string(secondBody) || !reflect.DeepEqual(firstHeaders, req.Header) {
				t.Fatal("request preparation applied twice")
			}
			sent := false
			transport := codexAlphaSearchRoundTripFunc(func(out *http.Request) (*http.Response, error) {
				sent = true
				wireBody, _ := io.ReadAll(out.Body)
				if string(wireBody) != string(firstBody) || !reflect.DeepEqual(firstHeaders, out.Header) {
					t.Fatal("HttpRequest remapped or changed the prepared request")
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"output":"ok"}`))}, nil
			})
			response, err := e.HttpRequest(context.WithValue(ctx, "cliproxy.roundtripper", http.RoundTripper(transport)), auth, req)
			if err != nil {
				t.Fatal(err)
			}
			_ = response.Body.Close()
			if !sent {
				t.Fatal("custom transport not used")
			}
		}
	}
}

func TestCodexAlphaSearchNativeOverridesAndGating(t *testing.T) {
	for _, kind := range []string{"native", "disabled", "api-key", "siwc", "other-path", "other-method"} {
		t.Run(kind, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Codex.HTTPTLSProfile = "linux"
			auth := &cliproxyauth.Auth{ID: "test", Provider: "codex", Metadata: map[string]any{"access_token": "synthetic", "account_id": "test"}, Attributes: map[string]string{}}
			method, path := "POST", "/backend-api/codex/alpha/search"
			switch kind {
			case "disabled":
				cfg.Codex.HTTPTLSProfile = "chrome"
			case "api-key":
				auth.Attributes["api_key"] = "synthetic"
			case "siwc":
				auth.Metadata["use_siwc"] = true
				auth.Metadata["siwc"] = map[string]any{"access_token": "synthetic"}
			case "other-path":
				path = "/other"
			case "other-method":
				method = "GET"
			}
			req, _ := http.NewRequest(method, "https://chatgpt.com"+path, strings.NewReader(`{"id":"original"}`))
			req.Header.Set("Originator", "original")
			req.Header.Set("Accept", "original")
			if kind == "native" {
				auth.Attributes = map[string]string{"header:User-Agent": "operator/1", "header:Originator": "operator", "header:Version": "operator-version", "header:Accept": "operator/type", "header:X-Operator": "keep"}
			}
			if err := NewCodexExecutor(cfg).PrepareRequest(req, auth); err != nil {
				t.Fatal(err)
			}
			if kind == "native" {
				if req.Header.Get("Originator") != "operator" || req.Header.Get("User-Agent") != "operator/1" || req.Header.Get("Version") != "operator-version" || req.Header.Get("Accept") != "operator/type" || req.Header.Get("X-Operator") != "keep" {
					t.Fatal("operator header override lost")
				}
			} else {
				reader, _ := req.GetBody()
				data, _ := io.ReadAll(reader)
				_ = reader.Close()
				if string(data) != `{"id":"original"}` || req.Header.Get("Originator") != "original" || req.Header.Get("Accept") != "original" {
					t.Fatal("ineligible request changed")
				}
			}
		})
	}
}

func TestCodexAlphaSearchNativeClientIdentity(t *testing.T) {
	cfg := &config.Config{}
	cfg.Codex.HTTPTLSProfile = "linux"
	cfg.Codex.DisableCodexCloaking = true
	auth := &cliproxyauth.Auth{ID: "test", Provider: "codex", Metadata: map[string]any{"access_token": "synthetic"}}
	req, _ := http.NewRequest("POST", "https://chatgpt.com/alpha/search", strings.NewReader(`{"commands":{"open":[{"ref_id":"https://example.invalid"}]}}`))
	req.Header = http.Header{"User-Agent": {"codex-tui/0.163.0 native"}, "Originator": {"Codex Desktop"}, "Version": {"0.163.0"}, "Accept": {"custom/client"}}
	if err := NewCodexExecutor(cfg).PrepareRequest(req, auth); err != nil {
		t.Fatal(err)
	}
	if req.Header.Get("User-Agent") != "codex-tui/0.163.0 native" || req.Header.Get("Originator") != "Codex Desktop" || req.Header.Get("Version") != "0.163.0" || req.Header.Get("Accept") != "custom/client" {
		t.Fatal("native identity did not pass through")
	}
	data, _ := io.ReadAll(req.Body)
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload) != 1 || payload["commands"] == nil || req.Header.Get("X-Codex-Turn-Metadata") != "" {
		t.Fatal("missing identifiers synthesized")
	}
}

func TestCodexAlphaSearchFinalizerAndAccountRetry(t *testing.T) {
	cfg := &config.Config{}
	cfg.Codex.HTTPTLSProfile = "linux"
	calls := 0
	ctx := helps.WithPayloadFinalizer(context.Background(), func(body []byte) []byte {
		calls++
		return helps.SetStringIfDifferent(body, "id", "operator-id")
	})
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://chatgpt.com/alpha/search", strings.NewReader(`{"id":"019a0992-1400-7abc-8def-0123456789ab","commands":{"open":[]}}`))
	e := NewCodexExecutor(cfg)
	auth := &cliproxyauth.Auth{ID: "a", Provider: "codex", Metadata: map[string]any{"access_token": "synthetic", "account_id": "a"}}
	for i := 0; i < 2; i++ {
		if err := e.PrepareRequest(req, auth); err != nil {
			t.Fatal(err)
		}
	}
	r, _ := req.GetBody()
	body, _ := io.ReadAll(r)
	_ = r.Close()
	if calls != 1 || gjson.GetBytes(body, "id").String() != "operator-id" {
		t.Fatal("payload finalizer was repeated or overwritten")
	}
	// A different account must rebuild from the original client body, then run
	// the finalizer exactly once for that attempt.
	other := &cliproxyauth.Auth{ID: "b", Provider: "codex", Metadata: map[string]any{"access_token": "synthetic", "account_id": "b"}}
	if err := e.PrepareRequest(req, other); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || req.Header.Get("Chatgpt-Account-Id") != "b" {
		t.Fatal("new account did not rebuild the attempt")
	}
}
