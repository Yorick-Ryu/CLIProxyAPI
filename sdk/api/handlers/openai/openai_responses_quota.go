package openai

import (
	"encoding/json"
	"strings"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
)

func (h *OpenAIResponsesAPIHandler) stripQuotaEnabled() bool {
	if h == nil || h.BaseAPIHandler == nil {
		return false
	}
	cfg := h.CurrentConfig()
	return cfg != nil && cfg.Client.Codex.StripQuota
}

// filterResponsesQuota only inspects events that can carry response headers.
// Ordinary text/tool deltas and disabled filtering keep the original bytes.
// Dedicated codex.rate_limits events are dropped by the transport callers.
func filterResponsesQuota(payload []byte, eventType string, enabled bool) []byte {
	if !enabled {
		return payload
	}
	switch eventType {
	case "codex.response.metadata", "response.metadata", "response.created",
		"response.in_progress", "response.completed", "response.failed", "response.incomplete", "error":
	default:
		return payload
	}
	var doc *ojson.Doc
	for _, path := range []string{"headers", "response.headers", "error.headers"} {
		headers := ojson.GetBytesNoCopy(payload, path)
		if !headers.IsObject() {
			continue
		}
		failed := false
		headers.ForEach(func(key, _ ojson.Result) bool {
			name := key.String()
			if !handlers.IsCodexQuotaHeader(name) {
				return true
			}
			if doc == nil {
				if !json.Valid(payload) {
					failed = true
					return false
				}
				doc = ojson.NewDoc(payload)
			}
			// Escape JSON path metacharacters in upstream-defined limit names.
			name = strings.ReplaceAll(strings.ReplaceAll(name, `\`, `\\`), ".", `\.`)
			if err := doc.Delete(path + "." + name); err != nil {
				failed = true
				return false
			}
			return true
		})
		if failed {
			return payload
		}
	}
	if doc == nil {
		return payload
	}
	return doc.Bytes()
}
