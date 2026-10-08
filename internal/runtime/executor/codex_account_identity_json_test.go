package executor

import (
	"bytes"
	"context"
	"strings"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// The pre-batching implementation is an oracle for byte-for-byte compatibility.
func legacyCodexAccountIdentityBody(ctx context.Context, auth *cliproxyauth.Auth, rawJSON []byte) ([]byte, codexAccountIdentityState) {
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

func TestCodexAccountIdentityBatchMatchesLegacy(t *testing.T) {
	auth := &cliproxyauth.Auth{ID: "test-account", Provider: "codex"}
	for _, input := range []string{
		`{ "input":[{"text":"keep whitespace"}], "client_metadata" : {"session_id":"session", "thread_id":"thread", "turn_id":"turn", "window_id":"window", "installation_id":"device"}, "prompt_cache_key" : "session" }`,
		`{"prompt_cache_key":"session","client_metadata":{"session-id":"session","session_id":"session","thread-id":"thread","turn-id":"turn","x-codex-turn-metadata":"{\"session_id\":\"session\"}"},"input":[1,2]}`,
		`{"client_metadata":null,"prompt_cache_key":123}`,
		`{"client_metadata":[],"prompt_cache_key":null}`,
		`{"client_metadata":{"session_id":" ","session_id":"second","thread_id":false},"prompt_cache_key":""}`,
		`{"client_metadata":{"session_id":"\u4e2d\u6587","unknown":{"keep":true}},"prompt_cache_key":" \u4e2d\u6587 "}`,
		`{"client_metadata":{"session_id":"first"},"client_metadata":{"session_id":"second"},"prompt_cache_key":"first"}`,
		`{"model":"test","input":[{"type":"message","content":[{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]}]}`,
	} {
		body := []byte(input)
		expected, expectedState := legacyCodexAccountIdentityBody(context.Background(), auth, body)
		got, state := applyCodexAccountIdentityBody(context.Background(), auth, body)
		if !bytes.Equal(got, expected) || state != expectedState {
			t.Fatalf("batch differs from legacy for %s\ngot %s\nwant %s", input, got, expected)
		}
		if string(body) != input {
			t.Fatal("source request changed")
		}
	}
}

func BenchmarkCodexAccountIdentityLargeJSON(b *testing.B) {
	body := []byte(`{"input":"` + strings.Repeat("x", 16<<20) + `","client_metadata":{"session_id":"session","thread_id":"thread","turn_id":"turn","window_id":"window","installation_id":"device"},"prompt_cache_key":"session"}`)
	auth := &cliproxyauth.Auth{ID: "test-account", Provider: "codex"}
	for _, tc := range []struct {
		name  string
		apply func(context.Context, *cliproxyauth.Auth, []byte) ([]byte, codexAccountIdentityState)
	}{{"legacy", legacyCodexAccountIdentityBody}, {"batched", applyCodexAccountIdentityBody}} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_, _ = tc.apply(context.Background(), auth, body)
			}
		})
	}
}
