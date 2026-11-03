package executor

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	auth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestCodexConvergenceRetainsTurnReferencesAcrossRetries(t *testing.T) {
	for _, mode := range []string{"session", "full"} {
		t.Run(mode, func(t *testing.T) {
			a := &auth.Auth{ID: "synthetic", Provider: "codex", Metadata: map[string]any{"account_id": "synthetic", codexIdentityConvergenceModeKey: mode}}
			const turn = "019a0992-1500-7abc-8def-0123456789ab"
			nested := map[string]any{"turn_id": turn, "root_turn_id": turn, "parent_turn_id": turn, "window_number": 7, "turn_started_at_unix_ms": 1234567890123}
			rawNested, _ := json.Marshal(nested)
			raw, _ := json.Marshal(map[string]any{"client_metadata": map[string]any{"session_id": turn, "turn_id": turn, "root_turn_id": turn, "parent_turn_id": turn, "window_number": "2", "x-codex-turn-metadata": string(rawNested)}})
			mapped, account := applyCodexAccountIdentityBody(context.Background(), a, raw)
			expected := ojson.GetBytes(mapped, "client_metadata.turn_id").String()
			for range 2 {
				output, state := applyCodexIdentityConvergenceBody(&config.Config{}, a, mapped, mapped, nil)
				m := ojson.Parse(ojson.GetBytes(output, "client_metadata.x-codex-turn-metadata").String())
				for _, key := range []string{"turn_id", "root_turn_id", "parent_turn_id"} {
					if m.Get(key).String() != expected || ojson.GetBytes(output, "client_metadata."+key).String() != expected {
						t.Fatalf("%s lost identity", key)
					}
				}
				assertCodexIdentityV7Timestamp(t, state.turnID, turn)
				if m.Get("turn_started_at_unix_ms").Int() != 1234567890123 {
					t.Fatal("client timestamp overwritten")
				}
				flat := ojson.GetBytes(output, "client_metadata.window_number")
				if flat.Type != ojson.String || flat.String() != "7" || m.Get("window_number").Int() != 7 {
					t.Fatal("window projections differ")
				}
				headers := http.Header{"X-Codex-Turn-Metadata": {string(rawNested)}}
				applyCodexAccountIdentityHeaders(headers, &account)
				applyCodexIdentityConvergenceHeaders(headers, &state)
				h := ojson.Parse(headers.Get("X-Codex-Turn-Metadata"))
				if h.Get("turn_id").String() != expected || h.Get("root_turn_id").String() != expected || h.Get("turn_started_at_unix_ms").Int() != 1234567890123 {
					t.Fatal("header lineage differs")
				}
			}
		})
	}
}

func TestCodexMemoryMetadataOmissionsSurviveConvergence(t *testing.T) {
	for _, mode := range []string{"device", "session", "full"} {
		a := &auth.Auth{ID: "synthetic", Provider: "codex", Metadata: map[string]any{codexIdentityConvergenceModeKey: mode}}
		raw := []byte(`{"client_metadata":{"session_id":"synthetic-session","x-codex-turn-metadata":"{\"request_kind\":\"memory\",\"extension\":\"keep\"}"}}`)
		output, state := applyCodexIdentityConvergenceBody(&config.Config{}, a, raw, raw, nil)
		headers := http.Header{"X-Codex-Turn-Metadata": {`{"request_kind":"memory","extension":"keep"}`}}
		applyCodexIdentityConvergenceHeaders(headers, &state)
		for _, blob := range []string{ojson.GetBytes(output, "client_metadata.x-codex-turn-metadata").String(), headers.Get("X-Codex-Turn-Metadata")} {
			for _, key := range []string{"installation_id", "session_id", "thread_id", "window_id", "window_number", "context_window_id", "turn_id", "turn_started_at_unix_ms"} {
				if ojson.Get(blob, key).Exists() {
					t.Fatalf("%s synthesized %s", mode, key)
				}
			}
			if ojson.Get(blob, "extension").String() != "keep" {
				t.Fatal("extension lost")
			}
		}
		if !ojson.GetBytes(output, "client_metadata.session_id").Exists() {
			t.Fatal("outer compatibility identity lost")
		}
	}
}
