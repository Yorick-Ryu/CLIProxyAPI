package openai

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
)

func (h *OpenAIResponsesAPIHandler) stripModelsETagEnabled() bool {
	if h == nil || h.BaseAPIHandler == nil {
		return false
	}
	cfg := h.CurrentConfig()
	return cfg != nil && cfg.Client.Codex.StripModelsETag
}

func writeResponsesUpstreamHeaders(dst, src http.Header, stripModelsETag bool) {
	handlers.WriteUpstreamHeaders(dst, src)
	if stripModelsETag {
		// Do not mutate the upstream map shared with tracing and interceptors.
		for key := range dst {
			if strings.EqualFold(key, "x-models-etag") {
				delete(dst, key)
			}
		}
	}
}

// filterResponsesModelsETag reuses the event type already decoded by the caller.
// Ordinary deltas and disabled filtering return the original slice without parsing
// or allocating. Only a metadata event with the target header needs a new document.
func filterResponsesModelsETag(payload []byte, eventType string, enabled bool) []byte {
	if !enabled || eventType != "codex.response.metadata" {
		return payload
	}
	headers := ojson.GetBytesNoCopy(payload, "headers")
	if !headers.IsObject() {
		return payload
	}
	var doc *ojson.Doc
	var failed bool
	headers.ForEach(func(key, _ ojson.Result) bool {
		name := key.String()
		if !strings.EqualFold(name, "x-models-etag") {
			return true
		}
		if doc == nil {
			if !json.Valid(payload) {
				return false
			}
			doc = ojson.NewDoc(payload)
		}
		if errDelete := doc.Delete("headers." + name); errDelete != nil {
			failed = true
			return false
		}
		return true
	})
	if doc == nil || failed {
		return payload
	}
	return doc.Bytes()
}
