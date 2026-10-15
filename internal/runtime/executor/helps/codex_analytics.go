package helps

import (
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ApplyCodexAnalyticsBody rewrites existing turn metadata before payload rules.
// Missing metadata and malformed values are preserved; valid objects may gain the flag.
func ApplyCodexAnalyticsBody(cfg *config.Config, body []byte) []byte {
	if cfg == nil || cfg.Codex.AnalyticsEnabled == nil || !gjson.ValidBytes(body) {
		return body
	}
	const path = "client_metadata.x-codex-turn-metadata"
	metadata := gjson.GetBytes(body, path)
	var updated []byte
	var err error
	switch {
	case metadata.Type == gjson.String:
		rewritten := rewriteCodexAnalyticsMetadata(metadata.Str, *cfg.Codex.AnalyticsEnabled)
		if rewritten == metadata.Str {
			return body
		}
		updated, err = sjson.SetBytes(body, path, rewritten)
	case metadata.IsObject():
		rewritten := rewriteCodexAnalyticsMetadata(metadata.Raw, *cfg.Codex.AnalyticsEnabled)
		if rewritten == metadata.Raw {
			return body
		}
		updated, err = sjson.SetRawBytes(body, path, []byte(rewritten))
	default:
		return body
	}
	if err != nil {
		return body
	}
	return updated
}

// ApplyCodexAnalyticsHeaders preserves header spelling, multiplicity, and all
// unrelated metadata. It does not create a header that the client omitted.
func ApplyCodexAnalyticsHeaders(cfg *config.Config, headers http.Header) {
	if cfg == nil || cfg.Codex.AnalyticsEnabled == nil {
		return
	}
	for key, values := range headers {
		if !strings.EqualFold(key, "X-Codex-Turn-Metadata") {
			continue
		}
		var rewritten []string
		for i, value := range values {
			updated := rewriteCodexAnalyticsMetadata(value, *cfg.Codex.AnalyticsEnabled)
			if updated == value {
				continue
			}
			if rewritten == nil {
				rewritten = append([]string(nil), values...)
			}
			rewritten[i] = updated
		}
		if rewritten != nil {
			headers[key] = rewritten
		}
	}
}

func rewriteCodexAnalyticsMetadata(metadata string, enabled bool) string {
	if !gjson.Valid(metadata) || !gjson.Parse(metadata).IsObject() {
		return metadata
	}
	value := gjson.Get(metadata, "analytics_enabled")
	if (value.Type == gjson.True || value.Type == gjson.False) && value.Bool() == enabled {
		return metadata
	}
	updated, err := sjson.Set(metadata, "analytics_enabled", enabled)
	if err != nil {
		return metadata
	}
	return updated
}
