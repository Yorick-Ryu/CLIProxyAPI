package executor

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// These snapshots preserve the DMIT implementation before this optimization.
func preBatchCodexAccountIdentityBody(ctx context.Context, auth *cliproxyauth.Auth, rawJSON []byte) ([]byte, codexAccountIdentityState) {
	state := resolveCodexAccountIdentityState(auth)
	if !state.enabled || len(rawJSON) == 0 || !util.ParseGJSONBytesNoCopy(rawJSON).IsObject() {
		return rawJSON, state
	}

	metadata := util.GetGJSONBytesNoCopy(rawJSON, "client_metadata")
	metadataJSON := metadata.Raw
	for _, field := range codexAccountIdentityBodyFields {
		path := strings.TrimPrefix(field.path, "client_metadata.")
		value := gjson.Get(metadataJSON, path)
		if value.Type != gjson.String || strings.TrimSpace(value.String()) == "" {
			continue
		}
		updated, err := sjson.Set(metadataJSON, path, codexAccountIdentityUUID(state, field.kind, value.String()))
		if err == nil {
			metadataJSON = updated
		}
	}

	if turnMetadata := strings.TrimSpace(gjson.Get(metadataJSON, "x-codex-turn-metadata").String()); turnMetadata != "" {
		metadataJSON, _ = sjson.Set(metadataJSON, "x-codex-turn-metadata", preBatchRemapCodexAccountIdentityJSON(turnMetadata, state))
	}

	var edits []helps.JSONValueReplacement
	if metadataJSON != metadata.Raw {
		edits = append(edits, helps.JSONValueReplacement{Value: metadata, Raw: []byte(metadataJSON)})
	}
	cacheValue := util.GetGJSONBytesNoCopy(rawJSON, "prompt_cache_key")
	promptCacheKey := strings.TrimSpace(cacheValue.String())
	if promptCacheKey != "" {
		state.originalPromptCacheKey = strings.Clone(promptCacheKey)
		// Forks can inherit a parent's cache key while carrying their own session
		// identity. The mapping must not depend on the current request's session.
		state.promptCacheKey = codexAccountIdentityUUID(state, "session", promptCacheKey)
		if state.promptCacheKey != promptCacheKey {
			edits = append(edits, helps.JSONValueReplacement{Value: cacheValue, Raw: []byte(`"` + state.promptCacheKey + `"`)})
			state.promptCacheKeyWasRemapped = true
		}
	}
	rawJSON, _ = helps.ReplaceJSONValues(rawJSON, edits)
	// History retains the turn that created each message, rather than adopting
	// the current request's turn. Use the same namespace as turn metadata.
	if input := gjson.GetBytes(rawJSON, "input"); input.IsArray() {
		for i, item := range input.Array() {
			value := item.Get("internal_chat_message_metadata_passthrough.turn_id")
			if value.Type != gjson.String || strings.TrimSpace(value.String()) == "" {
				continue
			}
			path := fmt.Sprintf("input.%d.internal_chat_message_metadata_passthrough.turn_id", i)
			updated, err := sjson.SetBytes(rawJSON, path, codexAccountIdentityUUID(state, "turn", value.String()))
			if err == nil {
				rawJSON = updated
			}
		}
	}
	return rawJSON, state
}

func preBatchCodexIdentityConvergenceBody(cfg *config.Config, auth *cliproxyauth.Auth, userPayload []byte, rawJSON []byte, clientHeaders http.Header) ([]byte, codexIdentityConvergenceState) {
	state := resolveCodexIdentityConvergenceState(cfg, auth, userPayload, rawJSON, clientHeaders)
	if !state.enabled || len(rawJSON) == 0 {
		return rawJSON, state
	}

	rawJSON = setCodexIdentityConvergenceBodyValue(rawJSON, "client_metadata.x-codex-installation-id", state.installationID)
	if state.mode != codexIdentityConvergenceDevice {
		rawJSON = setCodexIdentityConvergenceBodyValue(rawJSON, "client_metadata.session_id", state.sessionID)
		rawJSON = setCodexIdentityConvergenceBodyValue(rawJSON, "client_metadata.thread_id", state.threadID)
		rawJSON = setCodexIdentityConvergenceBodyValue(rawJSON, "client_metadata.turn_id", state.turnID)
		rawJSON = setCodexIdentityConvergenceBodyValue(rawJSON, "client_metadata.x-codex-window-id", state.windowID)
		if gjson.GetBytes(rawJSON, "client_metadata.window_id").Exists() {
			rawJSON = setCodexIdentityConvergenceBodyValue(rawJSON, "client_metadata.window_id", state.windowID)
		}
		if gjson.GetBytes(rawJSON, "client_metadata.window_number").Type == gjson.String {
			rawJSON = setCodexIdentityConvergenceBodyValue(rawJSON, "client_metadata.window_number", state.windowOrdinal)
		} else if gjson.GetBytes(rawJSON, "client_metadata.window_number").Type == gjson.Number {
			rawJSON, _ = sjson.SetRawBytes(rawJSON, "client_metadata.window_number", []byte(state.windowOrdinal))
		}
	}

	if turnMetadata := strings.TrimSpace(gjson.GetBytes(rawJSON, "client_metadata.x-codex-turn-metadata").String()); turnMetadata != "" {
		rewrittenMetadata := rewriteCodexIdentityConvergenceTurnMetadata(turnMetadata, state)
		rawJSON = setCodexIdentityConvergenceBodyValue(rawJSON, "client_metadata.x-codex-turn-metadata", rewrittenMetadata)
	}

	promptCacheKey := strings.TrimSpace(gjson.GetBytes(rawJSON, "prompt_cache_key").String())
	if state.mode != codexIdentityConvergenceDevice && state.originalBodySessionID != "" && promptCacheKey == state.originalBodySessionID {
		rawJSON = setCodexIdentityConvergenceBodyValue(rawJSON, "prompt_cache_key", state.sessionID)
		state.promptCacheKeyWasConverged = true
	}

	return rawJSON, state
}

func batchTestPayload(history int, textBytes int) []byte {
	var body strings.Builder
	body.WriteString(`{ "unknown" : 1e+09, "client_metadata" : {"session_id":"session","thread_id":"thread","turn_id":"0199c123-4567-7890-8123-456789abcdef","window_id":"thread:3","window_number":3,"x-codex-turn-metadata":"{\"turn_id\":\"0199c123-4567-7890-8123-456789abcdef\",\"request_kind\":\"foreground\",\"unknown\":{\"z\":1,\"a\":2},\"tool_namespaces_info\":[\"keep-body\"]}"}, "prompt_cache_key" : "session", "input" : [`)
	for i := 0; i < history; i++ {
		if i > 0 {
			body.WriteByte(',')
		}
		fmt.Fprintf(&body, `{"type":"message","content":"%s","internal_chat_message_metadata_passthrough":{"turn_id":"history-%d"}}`, strings.Repeat("x", textBytes), i)
	}
	body.WriteString(`] }`)
	return []byte(body.String())
}

func runBatchTestPipeline(body []byte, auth *cliproxyauth.Auth, cfg *config.Config, headers http.Header, before bool) ([]byte, http.Header) {
	var identity codexAccountIdentityState
	if before {
		body, identity = preBatchCodexAccountIdentityBody(context.Background(), auth, body)
	} else {
		body, identity = applyCodexAccountIdentityBody(context.Background(), auth, body)
	}
	var convergence codexIdentityConvergenceState
	if before {
		body, convergence = preBatchCodexIdentityConvergenceBody(cfg, auth, body, body, headers)
		body = helps.CanonicalCodexTurnMetadataBody(body)
	} else {
		body, convergence = applyCodexIdentityConvergenceBody(cfg, auth, body, body, headers)
		body = helps.CanonicalCodexTurnMetadataBody(body)
	}
	headers = headers.Clone()
	if before {
		preBatchCodexAccountIdentityHeaders(headers, &identity)
	} else {
		applyCodexAccountIdentityHeaders(headers, &identity)
	}
	applyCodexIdentityConvergenceHeaders(headers, &convergence)
	helps.CanonicalCodexTurnMetadataHeaders(headers)
	return body, headers
}

func TestCodexBodyBatchPipelineMatchesBefore(t *testing.T) {
	for _, mode := range []string{"off", "device", "session", "full"} {
		t.Run(mode, func(t *testing.T) {
			cfg := &config.Config{}
			credential := &cliproxyauth.Auth{ID: "batch-account", Provider: "codex", Metadata: map[string]any{codexIdentityConvergenceModeKey: mode}}
			body := batchTestPayload(32, 128)
			original := bytes.Clone(body)
			headers := http.Header{"Session-Id": {"session"}, "Thread-Id": {"thread"}, "X-Codex-Window-Id": {"thread:3"}, "X-Codex-Turn-Metadata": {`{"turn_id":"0199c123-4567-7890-8123-456789abcdef","unknown":"中文","tool_namespaces_info":["omit-header"]}`}, "X-Unrelated": {"keep", "both"}}
			headersBefore := headers.Clone()
			expected, expectedHeaders := runBatchTestPipeline(body, credential, cfg, headers, true)
			actual, actualHeaders := runBatchTestPipeline(body, credential, cfg, headers, false)
			if !bytes.Equal(actual, expected) {
				t.Fatalf("body differs\ngot %s\nwant %s", actual, expected)
			}
			if !reflect.DeepEqual(actualHeaders, expectedHeaders) {
				t.Fatalf("headers differ\ngot %v\nwant %v", actualHeaders, expectedHeaders)
			}
			if !bytes.Equal(body, original) || !reflect.DeepEqual(headers, headersBefore) {
				t.Fatal("source request mutated")
			}
		})
	}
}

func TestCodexHistoryBatchMatchesBefore(t *testing.T) {
	credential := &cliproxyauth.Auth{ID: "batch-account", Provider: "codex"}
	for _, body := range [][]byte{
		batchTestPayload(0, 0), batchTestPayload(1, 0), batchTestPayload(100, 16),
		[]byte(`{ "input" : [null, 1, "text", {"internal_chat_message_metadata_passthrough":{"turn_id":" 中文 "}}, {"internal_chat_message_metadata_passthrough":{"turn_id":null}}, {"internal_chat_message_metadata_passthrough":{"turn_id":"one","turn_id":"two"}}], "input":[], "client_metadata":{"session_id":"session"}}`),
	} {
		source := bytes.Clone(body)
		want, wantState := preBatchCodexAccountIdentityBody(context.Background(), credential, body)
		got, gotState := applyCodexAccountIdentityBody(context.Background(), credential, body)
		if !bytes.Equal(got, want) || gotState != wantState {
			t.Fatalf("history rewrite differs\ngot %s\nwant %s", got, want)
		}
		if !bytes.Equal(source, body) {
			t.Fatal("source mutated")
		}
	}
}

func BenchmarkCodexBodyBatchPipeline(b *testing.B) {
	for _, shape := range []struct {
		name        string
		turns, size int
	}{
		{"small", 4, 128}, {"history_128", 128, 1024}, {"large_2MiB", 64, 32768},
	} {
		body := batchTestPayload(shape.turns, shape.size)
		cfg := &config.Config{}
		credential := &cliproxyauth.Auth{ID: "batch-account", Provider: "codex", Metadata: map[string]any{codexIdentityConvergenceModeKey: "full"}}
		headers := http.Header{"Session-Id": {"session"}, "X-Codex-Turn-Metadata": {`{"turn_id":"0199c123-4567-7890-8123-456789abcdef"}`}}
		for _, before := range []bool{true, false} {
			label := "after"
			if before {
				label = "before"
			}
			b.Run(shape.name+"/"+label, func(b *testing.B) {
				b.ReportAllocs()
				b.SetBytes(int64(len(body)))
				for b.Loop() {
					runBatchTestPipeline(body, credential, cfg, headers, before)
				}
			})
		}
	}
}

func preBatchRemapCodexAccountIdentityJSON(raw string, state codexAccountIdentityState) string {
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

func preBatchCodexAccountIdentityHeaders(headers http.Header, state *codexAccountIdentityState) {
	if headers == nil || state == nil || !state.enabled {
		return
	}
	for _, field := range []struct {
		name string
		kind string
	}{
		{name: "X-Codex-Installation-Id", kind: "installation"},
		{name: "Thread-Id", kind: "thread"},
		{name: "X-Codex-Parent-Thread-Id", kind: "thread"},
		{name: "X-Codex-Window-Id", kind: "window"},
		// Native Codex uses its thread ID as the client request ID.
		{name: "X-Client-Request-Id", kind: "thread"},
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
		headers.Set("X-Codex-Turn-Metadata", preBatchRemapCodexAccountIdentityJSON(raw, *state))
	}
}

func TestCodexTurnMetadataBatchMatchesBefore(t *testing.T) {
	state := resolveCodexAccountIdentityState(&cliproxyauth.Auth{ID: "batch-account", Provider: "codex"})
	for _, raw := range []string{
		` { "session_id":"same", "thread_id":"same", "root_turn_id":"root", "parent_turn_id":"parent", "parent_thread_id":"parent-thread", "context_window_id":"context", "window_id":"same:12", "unknown":1e+09 } `,
		`{"turn_id":"one","turn_id":"two","session_id":"a\\b\"c","session-id":"中文"}`,
		`{"request_kind":"memory","unknown":{"keep":[1,null,true]}}`,
		`{"session_id":null,"turn_id":123,"thread_id":" "}`,
		`null`, `[]`, `"string"`, `{broken`, `{"session_id":"session","thread_id":"thread",broken`,
	} {
		want := preBatchRemapCodexAccountIdentityJSON(raw, state)
		got := remapCodexAccountIdentityJSON(raw, state)
		if got != want {
			t.Fatalf("metadata rewrite differs\ngot %s\nwant %s", got, want)
		}
	}
}

func TestCodexConvergenceBatchEdgeCasesMatchBefore(t *testing.T) {
	for _, mode := range []string{"device", "session", "full"} {
		credential := &cliproxyauth.Auth{ID: "batch-account", Provider: "codex", Metadata: map[string]any{codexIdentityConvergenceModeKey: mode}}
		cases := []string{
			` {"unknown":1e+09,"client_metadata" : {"turn_id":"0199c123-4567-7890-8123-456789abcdef"}, "prompt_cache_key":"session"} `,
			`{"client_metadata":{"turn_id":"0199c123-4567-7890-8123-456789abcdef","session_id":"session","window_number":"007","x-codex-turn-metadata":"{\"request_kind\":\"memory\",\"unknown\":true}"},"prompt_cache_key":"session"}`,
			`{"client_metadata":{"turn_id":"0199c123-4567-7890-8123-456789abcdef","session_id":"session","window_number":7,"x-codex-turn-metadata":{"keep":true}},"prompt_cache_key":123}`,
			`{"client_metadata":{"turn_id":"0199c123-4567-7890-8123-456789abcdef","x-codex-turn-metadata":"not-json"},"client_metadata":{"keep":"duplicate"},"prompt_cache_key":null}`,
		}
		if mode == "device" {
			cases = append(cases, `{}`, `{"client_metadata":null}`, `{"client_metadata":[]}`, `{"client_metadata":"text"}`)
		}
		for _, raw := range cases {
			body := []byte(raw)
			want, wantState := preBatchCodexIdentityConvergenceBody(&config.Config{}, credential, body, body, nil)
			got, gotState := applyCodexIdentityConvergenceBody(&config.Config{}, credential, body, body, nil)
			// Both implementations historically insert nested fields from a map.
			// Compare their canonical wire representation, not map iteration order.
			want = helps.CanonicalCodexTurnMetadataBody(want)
			got = helps.CanonicalCodexTurnMetadataBody(got)
			if !bytes.Equal(got, want) {
				t.Fatalf("mode %s\ngot %s\nwant %s", mode, got, want)
			}
			if mode != "device" && gotState != wantState {
				t.Fatal("convergence state differs")
			}
			if string(body) != raw {
				t.Fatal("source mutated")
			}
		}
	}
}
