package management

import (
	"context"
	"net/http"
	"strings"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// Quota probes do not pass through the generation executor. Feed a rejected
// stored OAuth token into the same recovery path so account-ops can observe it.
// The captured version/epoch prevent an old response condemning a new login.
func (h *Handler) observeCodexQuotaUnauthorized(ctx context.Context, route string, auth *coreauth.Auth, req *http.Request, resp *http.Response, token string) {
	if h == nil || h.authManager == nil || route != "/v8/management/requests/api-call" ||
		auth == nil || auth.Disabled || auth.Status == coreauth.StatusDisabled ||
		!strings.EqualFold(auth.Provider, "codex") || auth.AuthKind() == coreauth.AuthKindAPIKey ||
		strings.TrimSpace(auth.Attributes["api_key"]) != "" || token == "" ||
		resp == nil || resp.StatusCode != http.StatusUnauthorized || !codexQuotaRecoveryRequest(req) ||
		!codexQuotaRecoveryRequest(resp.Request) || req.URL.String() != resp.Request.URL.String() ||
		req.Header.Get("Authorization") != "Bearer "+token || resp.Request.Header.Get("Authorization") != "Bearer "+token {
		return
	}
	h.authManager.MarkResult(ctx, coreauth.Result{
		AuthID: auth.ID, Provider: "codex", CredentialScope: true, SkipQuotaObservation: true,
		AccessToken: token, CredentialVersion: auth.CredentialVersion, RegistrationEpoch: auth.RegistrationEpoch,
		Error: &coreauth.Error{HTTPStatus: http.StatusUnauthorized, Message: "401 unauthorized (Codex quota refresh)"},
	})
}

func codexQuotaRecoveryRequest(req *http.Request) bool {
	return req != nil && req.URL != nil && req.Method == http.MethodGet &&
		req.URL.Scheme == "https" && req.URL.Host == "chatgpt.com" &&
		(req.Host == "" || req.Host == "chatgpt.com") && req.URL.User == nil &&
		req.URL.Path == "/backend-api/wham/usage" && req.URL.RawQuery == "" && req.URL.Fragment == ""
}
