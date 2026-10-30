package helps

import (
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var codexTraceFields = [...]string{
	"traceparent", "tracestate",
	"ws_request_header_traceparent", "ws_request_header_tracestate",
}

var codexTraceMetadataFields = [...]string{"turnMetadata", "turn_metadata", "x-codex-turn-metadata"}

// stripCodexTraceObject removes only direct trace fields, never business input
// or arbitrary nested extensions. Duplicate trace keys are removed as well.
func stripCodexTraceObject(raw string) string {
	if !gjson.Parse(raw).IsObject() || !gjson.Valid(raw) {
		return raw
	}
	for _, key := range codexTraceFields {
		for gjson.Get(raw, key).Exists() {
			updated, err := sjson.Delete(raw, key)
			if err != nil || updated == raw {
				break
			}
			raw = updated
		}
	}
	return raw
}

// codexTraceEdit describes a replacement or a field deletion in the unchanged
// source object. Offsets include commas only for deletions.
type codexTraceEdit struct {
	start, end int
	raw        []byte
}

// rewriteCodexTraceObject collects field deletions and metadata replacements,
// then copies the enclosing object at most once. Unchanged objects reuse input.
func rewriteCodexTraceObject(body []byte, rewrite func(string, gjson.Result) []byte) []byte {
	object := util.ParseGJSONBytesNoCopy(body)
	if !object.IsObject() {
		return body
	}
	var edits []codexTraceEdit
	pending, pendingStart, previousEnd, lastEnd := false, 0, 0, 0
	object.ForEach(func(key, value gjson.Result) bool {
		remove := false
		for _, field := range codexTraceFields {
			if key.Str == field {
				remove = true
				break
			}
		}
		lastEnd = value.Index + len(value.Raw)
		if remove {
			if !pending {
				pending = true
				pendingStart = key.Index
			}
			return true
		}
		if pending {
			edits = append(edits, codexTraceEdit{start: pendingStart, end: key.Index})
			pending = false
		}
		if rewrite != nil {
			if raw := rewrite(key.Str, value); raw != nil && string(raw) != value.Raw {
				edits = append(edits, codexTraceEdit{start: value.Index, end: lastEnd, raw: raw})
			}
		}
		previousEnd = lastEnd
		return true
	})
	if pending {
		start := pendingStart
		if previousEnd != 0 {
			start = previousEnd
		}
		edits = append(edits, codexTraceEdit{start: start, end: lastEnd})
	}
	if len(edits) == 0 {
		return body
	}
	size, end := len(body), 0
	for _, edit := range edits {
		if edit.start < end || edit.end < edit.start || edit.end > len(body) {
			return body
		}
		size += len(edit.raw) - (edit.end - edit.start)
		end = edit.end
	}
	result := make([]byte, 0, size)
	end = 0
	for _, edit := range edits {
		result = append(result, body[end:edit.start]...)
		result = append(result, edit.raw...)
		end = edit.end
	}
	return append(result, body[end:]...)
}

func codexTraceMetadataReplacement(value gjson.Result, canonical bool) []byte {
	switch {
	case value.Type == gjson.String:
		updated := stripCodexTraceObject(value.Str)
		if canonical && validCodexMetadataString(value.Raw) {
			updated = CanonicalCodexTurnMetadata(updated)
		}
		if updated != value.Str {
			// Match sjson's existing string encoding on a small envelope, not the request.
			envelope, err := sjson.SetBytes([]byte(`{}`), "v", updated)
			if err == nil {
				return []byte(util.GetGJSONBytesNoCopy(envelope, "v").Raw)
			}
		}
	case value.IsObject():
		updated := stripCodexTraceObject(value.Raw)
		if canonical {
			updated = CanonicalCodexTurnMetadata(updated)
		}
		if updated != value.Raw {
			return []byte(updated)
		}
	}
	return nil
}

func codexTraceMetadataField(name string) bool {
	for _, field := range codexTraceMetadataFields {
		if name == field {
			return true
		}
	}
	return false
}

func prepareCodexTraceMetadataBody(body []byte, canonical bool) []byte {
	return rewriteCodexTraceObject(body, func(name string, value gjson.Result) []byte {
		if codexTraceMetadataField(name) {
			return codexTraceMetadataReplacement(value, false)
		}
		if name == "client_metadata" && value.IsObject() {
			raw := []byte(value.Raw)
			updated := rewriteCodexTraceObject(raw, func(field string, nested gjson.Result) []byte {
				if codexTraceMetadataField(field) {
					return codexTraceMetadataReplacement(nested, canonical && field == "x-codex-turn-metadata")
				}
				return nil
			})
			if string(updated) != value.Raw {
				return updated
			}
		}
		return nil
	})
}

// StripCodexTraceContextBody removes downstream trace correlation at known
// protocol locations, preserving business input and unrelated extensions.
// Call before explicit operator payload rules.
func StripCodexTraceContextBody(body []byte) []byte {
	return prepareCodexTraceMetadataBody(body, false)
}

// PrepareCodexRequestMetadata folds trace cleanup into the existing turn
// metadata formatting pass. Small metadata objects are edited first, and all
// replacements/deletions share at most one full-request allocation.
func PrepareCodexRequestMetadata(body []byte) []byte {
	return prepareCodexTraceMetadataBody(body, true)
}

// StripCodexTraceContextHeaders also handles mixed-case SDK headers and embedded
// turn metadata. Explicit account/model header overrides are applied afterwards.
func StripCodexTraceContextHeaders(headers http.Header) {
	for name, values := range headers {
		for _, key := range codexTraceFields {
			if strings.EqualFold(name, key) {
				delete(headers, name)
				break
			}
		}
		if strings.EqualFold(name, "x-codex-turn-metadata") {
			cleaned := append([]string(nil), values...)
			for i, raw := range cleaned {
				cleaned[i] = stripCodexTraceObject(raw)
			}
			headers[name] = cleaned
		}
	}
}
