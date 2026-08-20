package executor

import (
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// codexIdentityConvergenceState contains one outbound Codex device + session
// identity snapshot. One snapshot is shared by body and header rewriting so a
// request cannot expose conflicting turn metadata.
type codexIdentityConvergenceState struct {
	enabled                    bool
	installationID             string
	sessionID                  string
	threadID                   string
	turnID                     string
	windowID                   string
	turnStartedAtUnixMs        int64
	originalBodySessionID      string
	promptCacheKeyWasConverged bool
}

// codexIdentityConvergenceEnabled deliberately only applies to native Codex
// OAuth credentials. API-key upstreams preserve their caller-provided identity.
func codexIdentityConvergenceEnabled(cfg *config.Config, auth *cliproxyauth.Auth) bool {
	if cfg == nil || !cfg.Codex.IdentityConvergence || auth == nil || strings.TrimSpace(auth.ID) == "" {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return false
	}
	return !codexAuthUsesAPIKey(auth)
}

func codexIdentityConvergenceUUID(authID string, kind string, source string) string {
	name := strings.Join([]string{
		"cli-proxy-api",
		"codex",
		"identity-convergence",
		strings.TrimSpace(kind),
		strings.TrimSpace(authID),
		strings.TrimSpace(source),
	}, ":")
	id := uuid.NewSHA1(uuid.NameSpaceOID, []byte(name))
	id[6] = (id[6] & 0x0f) | 0x40
	id[8] = (id[8] & 0x3f) | 0x80
	return id.String()
}

func codexClientMetadataSessionID(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	return strings.TrimSpace(gjson.GetBytes(payload, "client_metadata.session_id").String())
}

func resolveCodexIdentityConvergenceState(cfg *config.Config, auth *cliproxyauth.Auth, userPayload []byte, rawJSON []byte, clientHeaders http.Header) codexIdentityConvergenceState {
	if !codexIdentityConvergenceEnabled(cfg, auth) {
		return codexIdentityConvergenceState{}
	}

	originalBodySessionID := codexClientMetadataSessionID(userPayload)
	if originalBodySessionID == "" {
		originalBodySessionID = codexClientMetadataSessionID(rawJSON)
	}
	clientSessionID := originalBodySessionID
	if clientSessionID == "" {
		clientSessionID = codexSessionHeaderValue(clientHeaders)
	}

	installationID := codexIdentityConvergenceUUID(auth.ID, "installation", "")
	sessionID := codexIdentityConvergenceUUID(auth.ID, "session", "")
	threadID := sessionID
	if clientSessionID != "" {
		threadID = codexIdentityConvergenceUUID(auth.ID, "thread", clientSessionID)
	}

	return codexIdentityConvergenceState{
		enabled:               true,
		installationID:        installationID,
		sessionID:             sessionID,
		threadID:              threadID,
		turnID:                uuid.Must(uuid.NewV7()).String(),
		windowID:              threadID + ":0",
		turnStartedAtUnixMs:   time.Now().UnixMilli(),
		originalBodySessionID: originalBodySessionID,
	}
}

// applyCodexIdentityConvergenceBody rewrites Codex client metadata to a stable
// device and session per OAuth auth. Threads remain client-session-specific so
// independent client sessions do not collapse into one upstream thread.
func applyCodexIdentityConvergenceBody(cfg *config.Config, auth *cliproxyauth.Auth, userPayload []byte, rawJSON []byte, clientHeaders http.Header) ([]byte, codexIdentityConvergenceState) {
	state := resolveCodexIdentityConvergenceState(cfg, auth, userPayload, rawJSON, clientHeaders)
	if !state.enabled || len(rawJSON) == 0 {
		return rawJSON, state
	}

	rawJSON = setCodexIdentityConvergenceBodyValue(rawJSON, "client_metadata.x-codex-installation-id", state.installationID)
	rawJSON = setCodexIdentityConvergenceBodyValue(rawJSON, "client_metadata.session_id", state.sessionID)
	rawJSON = setCodexIdentityConvergenceBodyValue(rawJSON, "client_metadata.thread_id", state.threadID)
	rawJSON = setCodexIdentityConvergenceBodyValue(rawJSON, "client_metadata.turn_id", state.turnID)
	rawJSON = setCodexIdentityConvergenceBodyValue(rawJSON, "client_metadata.x-codex-window-id", state.windowID)

	if turnMetadata := strings.TrimSpace(gjson.GetBytes(rawJSON, "client_metadata.x-codex-turn-metadata").String()); turnMetadata != "" {
		rewrittenMetadata := rewriteCodexIdentityConvergenceTurnMetadata(turnMetadata, state)
		rawJSON = setCodexIdentityConvergenceBodyValue(rawJSON, "client_metadata.x-codex-turn-metadata", rewrittenMetadata)
	}

	promptCacheKey := strings.TrimSpace(gjson.GetBytes(rawJSON, "prompt_cache_key").String())
	if state.originalBodySessionID != "" && promptCacheKey == state.originalBodySessionID {
		rawJSON = setCodexIdentityConvergenceBodyValue(rawJSON, "prompt_cache_key", state.sessionID)
		state.promptCacheKeyWasConverged = true
	}

	return rawJSON, state
}

func setCodexIdentityConvergenceBodyValue(rawJSON []byte, path string, value any) []byte {
	updated, err := sjson.SetBytes(rawJSON, path, value)
	if err != nil {
		return rawJSON
	}
	return updated
}

func rewriteCodexIdentityConvergenceTurnMetadata(raw string, state codexIdentityConvergenceState) string {
	if !state.enabled {
		return raw
	}
	if !gjson.Parse(raw).IsObject() {
		raw = "{}"
	}
	for path, value := range map[string]any{
		"installation_id":         state.installationID,
		"session_id":              state.sessionID,
		"thread_id":               state.threadID,
		"turn_id":                 state.turnID,
		"window_id":               state.windowID,
		"turn_started_at_unix_ms": state.turnStartedAtUnixMs,
	} {
		updated, err := sjson.Set(raw, path, value)
		if err != nil {
			continue
		}
		raw = updated
	}
	return raw
}

func applyCodexIdentityConvergenceHeaders(headers http.Header, state *codexIdentityConvergenceState) {
	if headers == nil || state == nil || !state.enabled {
		return
	}

	headers.Set("X-Codex-Installation-Id", state.installationID)
	setCodexSessionHeaderCasePreserved(headers, "Session-Id", state.sessionID)
	headers.Set("X-Client-Request-Id", state.threadID)
	headers.Set("Thread-Id", state.threadID)
	headers.Set("X-Codex-Window-Id", state.windowID)
	if rawTurnMetadata := strings.TrimSpace(headers.Get("X-Codex-Turn-Metadata")); rawTurnMetadata != "" {
		headers.Set("X-Codex-Turn-Metadata", rewriteCodexIdentityConvergenceTurnMetadata(rawTurnMetadata, *state))
	}
}
