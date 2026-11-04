package management

import (
	"context"
	"net/http"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestQuotaUnauthorizedRecovery(t *testing.T) {
	for _, name := range []string{"401", "success", "quota", "forbidden", "upstream", "legacy", "other_url", "host_override", "redirect", "no_stored_token", "other_token", "api_key", "other_provider", "disabled", "new_credentials"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			manager := coreauth.NewManager(nil, nil, nil)
			a := &coreauth.Auth{ID: "quota-" + name, Provider: "codex", Status: coreauth.StatusActive, Metadata: map[string]any{"access_token": "old-token"}}
			if name == "disabled" {
				a.Disabled = true
			}
			if name == "api_key" {
				a.Attributes = map[string]string{"api_key": "key"}
			}
			if name == "other_provider" {
				a.Provider = "claude"
			}
			a, err := manager.Register(ctx, a)
			if err != nil {
				t.Fatal(err)
			}
			h := &Handler{authManager: manager}
			req, _ := http.NewRequest("GET", "https://chatgpt.com/backend-api/wham/usage", nil)
			req.Header.Set("Authorization", "Bearer old-token")
			resp := &http.Response{StatusCode: 401, Request: req}
			route, token := "/v8/management/requests/api-call", "old-token"
			switch name {
			case "success":
				resp.StatusCode = 200
			case "quota":
				resp.StatusCode = 429
			case "forbidden":
				resp.StatusCode = 403
			case "upstream":
				resp.StatusCode = 503
			case "legacy":
				route = "/v0/management/api-call"
			case "other_url":
				req.URL.Path = "/backend-api/accounts/check/v4-2023-04-27"
			case "host_override":
				req.Host = "other.example"
			case "redirect":
				resp.Request, _ = http.NewRequest("GET", "https://other.example/", nil)
			case "no_stored_token":
				token = ""
			case "other_token":
				req.Header.Set("Authorization", "Bearer unrelated")
			case "new_credentials":
				updated := a.Clone()
				updated.Metadata = map[string]any{"access_token": "new-token"}
				if _, err := manager.Update(ctx, updated); err != nil {
					t.Fatal(err)
				}
			}
			h.observeCodexQuotaUnauthorized(ctx, route, a, req, resp, token)
			current, _ := manager.GetByID(a.ID)
			if name == "401" {
				if current.Status != coreauth.StatusError || !current.Unavailable || current.LastError == nil || current.LastError.HTTPStatus != 401 {
					t.Fatal("quota 401 did not reach the credential recovery state")
				}
			} else if current.Failed != 0 || current.LastError != nil {
				t.Fatal("unrelated or stale quota result changed credential health")
			}
		})
	}
}
