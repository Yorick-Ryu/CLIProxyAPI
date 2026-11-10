package handlers

import (
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
)

// IsCodexQuotaHeader identifies upstream account quota information, not response
// token usage, routing metadata, or Retry-After. Namespaced limits are supported.
func IsCodexQuotaHeader(name string) bool {
	name = strings.ToLower(name)
	if !strings.HasPrefix(name, "x-codex-") {
		return false
	}
	switch name {
	case "x-codex-active-limit", "x-codex-plan-type", "x-codex-rate-limit-reached-type":
		return true
	}
	if strings.HasPrefix(name, "x-codex-credits-") {
		return true
	}
	for _, suffix := range []string{
		"-used-percent", "-reset-at", "-window-minutes", "-reset-after-seconds",
		"-allowed", "-limit-reached", "-limit-name", "-over-secondary-limit-percent",
	} {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

// StripCodexQuotaError copies only client-facing header maps. The original error
// and upstream observations remain available to logging and scheduling.
func StripCodexQuotaError(msg *interfaces.ErrorMessage, enabled bool) *interfaces.ErrorMessage {
	if !enabled || msg == nil {
		return msg
	}
	filtered := *msg
	clean := func(src http.Header) http.Header {
		var dst http.Header
		for name := range src {
			if IsCodexQuotaHeader(name) {
				if dst == nil {
					dst = src.Clone()
				}
				delete(dst, name)
			}
		}
		if dst != nil {
			return dst
		}
		return src
	}
	filtered.Addon = clean(msg.Addon)
	filtered.Headers = clean(msg.Headers)
	return &filtered
}
