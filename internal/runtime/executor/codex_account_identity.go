package executor

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const codexAccountIdentityNamespaceVersion = "v1"

// codexAccountIdentityState carries only an irreversible namespace. It never
// retains the downstream API key or upstream access token.
type codexAccountIdentityState struct {
	enabled                   bool
	namespace                 string
	originalPromptCacheKey    string
	promptCacheKey            string
	promptCacheKeyWasRemapped bool
}

func codexCredentialIdentitySource(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Metadata != nil {
		accountID, _ := auth.Metadata["account_id"].(string)
		accountID = strings.TrimSpace(accountID)
		if accountID == "" {
			accountID, _ = auth.Metadata["chatgpt_account_id"].(string)
			accountID = strings.TrimSpace(accountID)
		}
		if accountID != "" {
			userID, _ := auth.Metadata["chatgpt_user_id"].(string)
			if userID = strings.TrimSpace(userID); userID != "" {
				return "account:" + accountID + ":user:" + userID
			}
			return "account:" + accountID
		}
	}
	if authID := strings.TrimSpace(auth.ID); authID != "" {
		return "auth:" + authID
	}
	return ""
}

func resolveCodexAccountIdentityState(ctx context.Context, auth *cliproxyauth.Auth) codexAccountIdentityState {
	if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") || codexAuthUsesAPIKey(auth) {
		return codexAccountIdentityState{}
	}
	credentialSource := codexCredentialIdentitySource(auth)
	if credentialSource == "" {
		return codexAccountIdentityState{}
	}
	callerSource := strings.TrimSpace(helps.APIKeyFromContext(ctx))
	if callerSource == "" {
		callerSource = "anonymous"
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{
		"cli-proxy-api",
		"codex-account-identity",
		codexAccountIdentityNamespaceVersion,
		"caller",
		callerSource,
		"credential",
		credentialSource,
	}, ":")))
	return codexAccountIdentityState{
		enabled:   true,
		namespace: fmt.Sprintf("%x", sum[:16]),
	}
}

func codexAccountIdentityUUID(state codexAccountIdentityState, kind, raw string) string {
	raw = strings.TrimSpace(raw)
	if !state.enabled || state.namespace == "" || raw == "" {
		return raw
	}
	name := strings.Join([]string{
		"cli-proxy-api",
		"codex-account-identity",
		codexAccountIdentityNamespaceVersion,
		state.namespace,
		strings.TrimSpace(kind),
		raw,
	}, ":")
	id := uuid.NewSHA1(uuid.NameSpaceOID, []byte(name))
	id[6] = (id[6] & 0x0f) | 0x40
	id[8] = (id[8] & 0x3f) | 0x80
	return id.String()
}

var codexAccountIdentityBodyFields = []struct {
	path string
	kind string
}{
	{path: "client_metadata.installation_id", kind: "installation"},
	{path: "client_metadata.x-codex-installation-id", kind: "installation"},
	{path: "client_metadata.session_id", kind: "session"},
	{path: "client_metadata.session-id", kind: "session"},
	{path: "client_metadata.thread_id", kind: "thread"},
	{path: "client_metadata.thread-id", kind: "thread"},
	{path: "client_metadata.turn_id", kind: "turn"},
	{path: "client_metadata.turn-id", kind: "turn"},
	{path: "client_metadata.window_id", kind: "window"},
	{path: "client_metadata.x-codex-window-id", kind: "window"},
	{path: "client_metadata.x-client-request-id", kind: "request"},
}

var codexAccountIdentityTurnMetadataFields = []struct {
	path string
	kind string
}{
	{path: "installation_id", kind: "installation"},
	{path: "x-codex-installation-id", kind: "installation"},
	{path: "session_id", kind: "session"},
	{path: "session-id", kind: "session"},
	{path: "thread_id", kind: "thread"},
	{path: "thread-id", kind: "thread"},
	{path: "turn_id", kind: "turn"},
	{path: "turn-id", kind: "turn"},
	{path: "window_id", kind: "window"},
	{path: "x-codex-window-id", kind: "window"},
	{path: "x-client-request-id", kind: "request"},
}

func remapCodexAccountIdentityJSON(raw string, state codexAccountIdentityState) string {
	if !state.enabled || !gjson.Parse(raw).IsObject() {
		return raw
	}
	for _, field := range codexAccountIdentityTurnMetadataFields {
		value := gjson.Get(raw, field.path)
		if value.Type != gjson.String || strings.TrimSpace(value.String()) == "" {
			continue
		}
		updated, err := sjson.Set(raw, field.path, codexAccountIdentityUUID(state, field.kind, value.String()))
		if err == nil {
			raw = updated
		}
	}
	return raw
}

// applyCodexAccountIdentityBody remaps existing client-owned identifiers into
// a caller-and-credential namespace. It preserves identity cardinality and does
// not synthesize fields that the client omitted.
func applyCodexAccountIdentityBody(ctx context.Context, auth *cliproxyauth.Auth, rawJSON []byte) ([]byte, codexAccountIdentityState) {
	state := resolveCodexAccountIdentityState(ctx, auth)
	if !state.enabled || len(rawJSON) == 0 || !gjson.ParseBytes(rawJSON).IsObject() {
		return rawJSON, state
	}

	originalSessionID := strings.TrimSpace(gjson.GetBytes(rawJSON, "client_metadata.session_id").String())
	if originalSessionID == "" {
		originalSessionID = strings.TrimSpace(gjson.GetBytes(rawJSON, "client_metadata.session-id").String())
	}
	for _, field := range codexAccountIdentityBodyFields {
		value := gjson.GetBytes(rawJSON, field.path)
		if value.Type != gjson.String || strings.TrimSpace(value.String()) == "" {
			continue
		}
		updated, err := sjson.SetBytes(rawJSON, field.path, codexAccountIdentityUUID(state, field.kind, value.String()))
		if err == nil {
			rawJSON = updated
		}
	}

	if turnMetadata := strings.TrimSpace(gjson.GetBytes(rawJSON, "client_metadata.x-codex-turn-metadata").String()); turnMetadata != "" {
		rawJSON, _ = sjson.SetBytes(rawJSON, "client_metadata.x-codex-turn-metadata", remapCodexAccountIdentityJSON(turnMetadata, state))
	}

	promptCacheKey := strings.TrimSpace(gjson.GetBytes(rawJSON, "prompt_cache_key").String())
	if promptCacheKey != "" {
		state.originalPromptCacheKey = promptCacheKey
		kind := "prompt-cache"
		if originalSessionID != "" && promptCacheKey == originalSessionID {
			kind = "session"
		}
		state.promptCacheKey = codexAccountIdentityUUID(state, kind, promptCacheKey)
		if state.promptCacheKey != promptCacheKey {
			rawJSON, _ = sjson.SetBytes(rawJSON, "prompt_cache_key", state.promptCacheKey)
			state.promptCacheKeyWasRemapped = true
		}
	}
	return rawJSON, state
}

func applyCodexAccountIdentityBodyForConfig(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, rawJSON []byte) ([]byte, codexAccountIdentityState) {
	// Legacy identity-confuse has reversible response mappings. Preserve that
	// contract when it is the active policy; convergence takes precedence over
	// identity-confuse and can safely compose with account scoping.
	if codexIdentityConfuseEnabled(cfg) && !codexIdentityConvergenceEnabled(cfg, auth) {
		return rawJSON, codexAccountIdentityState{}
	}
	return applyCodexAccountIdentityBody(ctx, auth, rawJSON)
}

func codexAccountIdentitySessionValue(state codexAccountIdentityState, raw string) string {
	raw = strings.TrimSpace(raw)
	if state.promptCacheKeyWasRemapped && raw == state.originalPromptCacheKey {
		return state.promptCacheKey
	}
	return codexAccountIdentityUUID(state, "session", raw)
}

func applyCodexAccountIdentityHeaders(headers http.Header, state *codexAccountIdentityState) {
	if headers == nil || state == nil || !state.enabled {
		return
	}
	for _, field := range []struct {
		name string
		kind string
	}{
		{name: "X-Codex-Installation-Id", kind: "installation"},
		{name: "Thread-Id", kind: "thread"},
		{name: "X-Codex-Window-Id", kind: "window"},
		{name: "X-Client-Request-Id", kind: "request"},
	} {
		raw := strings.TrimSpace(headerValueCaseInsensitive(headers, field.name))
		if raw != "" {
			setHeaderCasePreserved(headers, field.name, codexAccountIdentityUUID(*state, field.kind, raw))
		}
	}
	if raw := codexSessionHeaderValue(headers); raw != "" {
		setCodexSessionHeaderCasePreserved(headers, "Session-Id", codexAccountIdentitySessionValue(*state, raw))
	}
	for _, name := range []string{"Conversation-Id", "Conversation_id"} {
		if raw := strings.TrimSpace(headerValueCaseInsensitive(headers, name)); raw != "" {
			setHeaderCasePreserved(headers, name, codexAccountIdentitySessionValue(*state, raw))
		}
	}
	if raw := strings.TrimSpace(headers.Get("X-Codex-Turn-Metadata")); raw != "" {
		headers.Set("X-Codex-Turn-Metadata", remapCodexAccountIdentityJSON(raw, *state))
	}
}
