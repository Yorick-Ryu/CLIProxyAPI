package helps

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"golang.org/x/net/http/httpguts"
)

// Field layouts follow openai/codex 3fd08b540570a5dd9c61d98560a0231f7b43891f:
// core/src/responses_metadata.rs and utils/string/src/json.rs. Struct fields
// retain declaration order; BTreeMap keys and unknown extensions sort by name.
var codexTurnMetadataOrder = []string{
	"installation_id", "session_id", "thread_id", "agent_name", "turn_id",
	"window_id", "window_number", "context_window_id", "request_kind",
	"history_initialization", "forked_from_thread_id", "forked_from_ordinal_exclusive",
	"parent_thread_id", "parent_turn_id", "root_turn_id", "subagent_kind",
	"thread_source", "turn_trigger", "sandbox", "sandbox_mode", "auto_review_enabled",
	"node_repl_auto_review_required", "node_repl_disabled", "workspaces",
	"tool_namespaces_info", "turn_started_at_unix_ms", "history_ingest_requested",
	"analytics_enabled", "compaction",
}

type codexMetadataShape uint8

const (
	codexMetadataMap codexMetadataShape = iota
	codexMetadataRoot
	codexMetadataWorkspaces
	codexMetadataWorkspace
	codexMetadataNamespaces
	codexMetadataNamespace
	codexMetadataFunctions
	codexMetadataFunction
	codexMetadataToolSource
	codexMetadataCompaction
)

func codexMetadataFieldOrder(shape codexMetadataShape) []string {
	switch shape {
	case codexMetadataRoot:
		return codexTurnMetadataOrder
	case codexMetadataWorkspace:
		return []string{"associated_remote_urls", "latest_git_commit_hash", "has_changes"}
	case codexMetadataNamespace:
		return []string{"name", "functions"}
	case codexMetadataFunction:
		return []string{"name", "direct", "code_mode_name", "deferred", "source"}
	case codexMetadataToolSource:
		return []string{"kind", "server_name"}
	case codexMetadataCompaction:
		return []string{"trigger", "reason", "implementation", "phase", "strategy"}
	default:
		return nil
	}
}

func codexMetadataChildShape(shape codexMetadataShape, field string) codexMetadataShape {
	switch shape {
	case codexMetadataRoot:
		switch field {
		case "workspaces":
			return codexMetadataWorkspaces
		case "tool_namespaces_info":
			return codexMetadataNamespaces
		case "compaction":
			return codexMetadataCompaction
		}
	case codexMetadataWorkspaces:
		return codexMetadataWorkspace
	case codexMetadataNamespaces:
		return codexMetadataNamespace
	case codexMetadataNamespace:
		if field == "functions" {
			return codexMetadataFunctions
		}
	case codexMetadataFunctions:
		return codexMetadataFunction
	case codexMetadataFunction:
		if field == "source" {
			return codexMetadataToolSource
		}
	}
	return codexMetadataMap
}

// CanonicalCodexTurnMetadata matches native serde field order and ASCII JSON
// escaping without changing field values or synthesizing omitted fields. Raw
// numeric tokens stay lossless. Invalid and ambiguous duplicate-key input is
// passed through for upstream validation.
func CanonicalCodexTurnMetadata(raw string) string {
	return canonicalCodexTurnMetadata(raw, false)
}

func canonicalCodexTurnMetadata(raw string, header bool) string {
	if !utf8.ValidString(raw) || !gjson.Valid(raw) {
		return raw
	}
	value := gjson.Parse(raw)
	if !value.IsObject() {
		return raw
	}
	var output strings.Builder
	output.Grow(len(raw))
	if !writeCodexMetadataValue(&output, value, codexMetadataRoot) {
		return raw
	}
	canonical := output.String()
	if header {
		// Native compatibility headers omit the unbounded tool inventory.
		// Validate the entire original first, including duplicate keys and UTF-8.
		if projected, err := sjson.Delete(canonical, "tool_namespaces_info"); err == nil {
			canonical = projected
		}
	}
	return canonical
}

func writeCodexMetadataValue(output *strings.Builder, value gjson.Result, shape codexMetadataShape) bool {
	switch {
	case value.IsObject():
		fields := make(map[string]gjson.Result)
		unique := true
		value.ForEach(func(key, child gjson.Result) bool {
			if _, exists := fields[key.Str]; exists || !validCodexMetadataString(key.Raw) {
				unique = false
				return false
			}
			fields[key.Str] = child
			return true
		})
		if !unique {
			return false
		}
		keys := make([]string, 0, len(fields))
		for _, key := range codexMetadataFieldOrder(shape) {
			if _, exists := fields[key]; exists {
				keys = append(keys, key)
			}
		}
		known := len(keys)
		for key := range fields {
			found := false
			for _, present := range keys[:known] {
				if key == present {
					found = true
					break
				}
			}
			if !found {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys[known:])
		output.WriteByte('{')
		for index, key := range keys {
			if index > 0 {
				output.WriteByte(',')
			}
			writeCodexMetadataString(output, key)
			output.WriteByte(':')
			if !writeCodexMetadataValue(output, fields[key], codexMetadataChildShape(shape, key)) {
				return false
			}
		}
		output.WriteByte('}')
	case value.IsArray():
		output.WriteByte('[')
		valid := true
		first := true
		value.ForEach(func(_, child gjson.Result) bool {
			if !first {
				output.WriteByte(',')
			}
			first = false
			valid = writeCodexMetadataValue(output, child, codexMetadataMap)
			return valid
		})
		output.WriteByte(']')
		return valid
	case value.Type == gjson.String:
		if !validCodexMetadataString(value.Raw) {
			return false
		}
		writeCodexMetadataString(output, value.Str)
	default:
		// Do not convert i64/u64 timestamps, counts or extension decimals to float64.
		output.WriteString(value.Raw)
	}
	return true
}

// GJSON accepts unpaired UTF-16 escapes and replaces them while decoding. Native
// serde rejects them; leave such input untouched rather than corrupting values.
func validCodexMetadataString(raw string) bool {
	for index := 1; index < len(raw)-1; index++ {
		if raw[index] != '\\' {
			continue
		}
		index++
		if raw[index] != 'u' {
			continue
		}
		code, _ := strconv.ParseUint(raw[index+1:index+5], 16, 16)
		index += 4
		switch {
		case code >= 0xd800 && code <= 0xdbff:
			if index+7 >= len(raw) || raw[index+1:index+3] != `\u` {
				return false
			}
			low, err := strconv.ParseUint(raw[index+3:index+7], 16, 16)
			if err != nil || low < 0xdc00 || low > 0xdfff {
				return false
			}
			index += 6
		case code >= 0xdc00 && code <= 0xdfff:
			return false
		}
	}
	return true
}

func writeCodexMetadataString(output *strings.Builder, value string) {
	const hex = "0123456789abcdef"
	writeUTF16 := func(code uint16) {
		output.WriteString(`\u`)
		output.WriteByte(hex[code>>12])
		output.WriteByte(hex[code>>8&15])
		output.WriteByte(hex[code>>4&15])
		output.WriteByte(hex[code&15])
	}
	output.WriteByte('"')
	for _, character := range value {
		switch character {
		case '"', '\\':
			output.WriteByte('\\')
			output.WriteByte(byte(character))
		case '\b':
			output.WriteString(`\b`)
		case '\f':
			output.WriteString(`\f`)
		case '\n':
			output.WriteString(`\n`)
		case '\r':
			output.WriteString(`\r`)
		case '\t':
			output.WriteString(`\t`)
		default:
			switch {
			case character < 0x20:
				writeUTF16(uint16(character))
			case character <= 0x7f:
				output.WriteByte(byte(character))
			case character <= 0xffff:
				writeUTF16(uint16(character))
			default:
				high, low := utf16.EncodeRune(character)
				writeUTF16(uint16(high))
				writeUTF16(uint16(low))
			}
		}
	}
	output.WriteByte('"')
}

// CanonicalCodexTurnMetadataBody formats only existing embedded metadata,
// preserving its string/object representation and all unrelated request bytes.
// Call before payload rules so explicit administrator overrides stay final.
func CanonicalCodexTurnMetadataBody(body []byte) []byte {
	const path = "client_metadata.x-codex-turn-metadata"
	metadata := gjson.GetBytes(body, path)
	var updated []byte
	var err error
	if metadata.Type == gjson.String {
		if !validCodexMetadataString(metadata.Raw) {
			return body
		}
		canonical := CanonicalCodexTurnMetadata(metadata.Str)
		if canonical == metadata.Str {
			return body
		}
		updated, err = sjson.SetBytes(body, path, canonical)
	} else if metadata.IsObject() {
		canonical := CanonicalCodexTurnMetadata(metadata.Raw)
		if canonical == metadata.Raw {
			return body
		}
		updated, err = sjson.SetRawBytes(body, path, []byte(canonical))
	} else {
		return body
	}
	if err != nil {
		return body
	}
	return updated
}

// CanonicalCodexTurnMetadataHeaders runs after identity rewrites.
// It retains header casing and multiplicity without mutating shared value slices.
func CanonicalCodexTurnMetadataHeaders(headers http.Header) {
	for key, values := range headers {
		if !strings.EqualFold(key, "X-Codex-Turn-Metadata") {
			continue
		}
		var updated []string
		for index, value := range values {
			canonical := canonicalCodexTurnMetadata(value, true)
			// Native ASCII JSON can retain DEL, which HeaderValue rejects.
			// Preserve a valid incoming projection if canonicalization would
			// make it untransmittable; never introduce an HTTP header error.
			if canonical != value && httpguts.ValidHeaderFieldValue(canonical) {
				if updated == nil {
					updated = append([]string(nil), values...)
				}
				updated[index] = canonical
			}
		}
		if updated != nil {
			headers[key] = updated
		}
	}
}
