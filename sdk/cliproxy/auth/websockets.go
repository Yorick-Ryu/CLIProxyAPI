package auth

import (
	"strconv"
	"strings"
)

// WebsocketsEnabled resolves an explicit credential setting before the supplied default.
func (auth *Auth) WebsocketsEnabled(defaultEnabled bool) bool {
	if auth == nil {
		return false
	}
	if len(auth.Attributes) > 0 {
		if raw := strings.TrimSpace(auth.Attributes["websockets"]); raw != "" {
			parsed, errParse := strconv.ParseBool(raw)
			if errParse == nil {
				return parsed
			}
		}
	}
	if len(auth.Metadata) == 0 {
		return defaultEnabled
	}
	raw, ok := auth.Metadata["websockets"]
	if !ok || raw == nil {
		return defaultEnabled
	}
	switch v := raw.(type) {
	case bool:
		return v
	case string:
		parsed, errParse := strconv.ParseBool(strings.TrimSpace(v))
		if errParse == nil {
			return parsed
		}
	default:
	}
	return defaultEnabled
}

// applyCodexWebsocketsDefault uses runtime state only; inherited values must never
// become persistent per-account overrides. The caller holds m.mu.
func (m *Manager) applyCodexWebsocketsDefault(auth *Auth) {
	if auth != nil {
		auth.codexWebsocketsDefaultDisabled = !m.runtimeConfigSnapshot().CodexWebsocketsDefault()
	}
}

func (m *Manager) reloadCodexWebsocketsDefault() {
	m.mu.Lock()
	var changed []*Auth
	for _, auth := range m.auths {
		if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
			continue
		}
		before := auth.codexWebsocketsDefaultDisabled
		m.applyCodexWebsocketsDefault(auth)
		if before != auth.codexWebsocketsDefaultDisabled {
			auth.Generation++
			changed = append(changed, auth.Clone())
		}
	}
	m.mu.Unlock()
	// Upstream scheduler sync only checks structural and registry epochs;
	// publish changed transport preferences through its incremental update path.
	if m.scheduler != nil {
		for _, auth := range changed {
			m.scheduler.upsertAuth(auth)
		}
	}
	if len(changed) > 0 {
		m.structuralEpoch.Add(1)
	}
	m.syncScheduler()
}

// EffectiveWebsocketsEnabled reports the transport preference from this runtime
// snapshot. Codex inherits its configured default; other providers default off.
func (auth *Auth) EffectiveWebsocketsEnabled() bool {
	return auth.WebsocketsEnabled(auth != nil && strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") && !auth.codexWebsocketsDefaultDisabled)
}
