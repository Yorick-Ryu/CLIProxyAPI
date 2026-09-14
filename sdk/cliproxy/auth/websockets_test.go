package auth

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestWebsocketsCredentialOverrides(t *testing.T) {
	for _, fallback := range []bool{true, false} {
		for _, tc := range []struct {
			name     string
			attrs    map[string]string
			metadata map[string]any
			want     bool
		}{
			{"missing", nil, nil, fallback},
			{"null", nil, map[string]any{"websockets": nil}, fallback},
			{"true", nil, map[string]any{"websockets": true}, true},
			{"false", nil, map[string]any{"websockets": false}, false},
			{"string false", nil, map[string]any{"websockets": "false"}, false},
			{"attribute wins", map[string]string{"websockets": "false"}, map[string]any{"websockets": true}, false},
			{"attribute true", map[string]string{"websockets": "true"}, nil, true},
			{"invalid", nil, map[string]any{"websockets": 1}, fallback},
		} {
			t.Run(tc.name, func(t *testing.T) {
				auth := &Auth{Provider: "codex", Attributes: tc.attrs, Metadata: tc.metadata}
				if got := auth.WebsocketsEnabled(fallback); got != tc.want {
					t.Fatalf("got %t, want %t (default %t)", got, tc.want, fallback)
				}
			})
		}
	}
	if (*Auth)(nil).WebsocketsEnabled(true) {
		t.Fatal("nil auth enabled")
	}
	if (&Auth{Provider: "xai"}).EffectiveWebsocketsEnabled() {
		t.Fatal("changed xAI default")
	}
}

func TestCodexWebsocketsDefaultReloadAndScheduler(t *testing.T) {
	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
	manager := NewManager(nil, nil, nil)
	for _, auth := range []*Auth{
		{ID: "inherited", Provider: "codex", Status: StatusActive},
		{ID: "disabled", Provider: "codex", Status: StatusActive, Metadata: map[string]any{"websockets": false}},
		{ID: "enabled", Provider: "codex", Status: StatusActive, Metadata: map[string]any{"websockets": true}},
	} {
		if _, err := manager.Register(ctx, auth); err != nil {
			t.Fatal(err)
		}
	}
	registerSchedulerModels(t, "codex", "ws-default-test", "inherited", "disabled", "enabled")
	manager.RefreshSchedulerAll()
	for _, enabled := range []bool{false, true, false} {
		manager.SetConfig(&config.Config{Codex: config.CodexConfig{WebsocketsDefault: &enabled}})
		auth, ok := manager.GetByID("inherited")
		if !ok {
			t.Fatal("missing auth")
		}
		if auth.EffectiveWebsocketsEnabled() != enabled {
			t.Fatal("reload did not update inherited preference")
		}
		if _, exists := auth.Metadata["websockets"]; exists {
			t.Fatal("default persisted as metadata")
		}
		if _, exists := auth.Attributes["websockets"]; exists {
			t.Fatal("default persisted as attribute")
		}
		data, err := json.Marshal(auth)
		if err != nil {
			t.Fatal(err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatal(err)
		}
		if _, exists := decoded["codexWebsocketsDefaultDisabled"]; exists {
			t.Fatal("runtime default serialized")
		}
		for i := 0; i < 4; i++ {
			picked, err := manager.scheduler.pickSingle(ctx, "codex", "ws-default-test", cliproxyexecutor.Options{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if picked.ID == "disabled" || (!enabled && picked.ID != "enabled") {
				t.Fatalf("scheduler selected %s with default %t", picked.ID, enabled)
			}
		}
		auth.UpdatedAt = time.Now()
		updated, err := manager.Update(ctx, auth)
		if err != nil {
			t.Fatal(err)
		}
		if updated.EffectiveWebsocketsEnabled() != enabled {
			t.Fatal("update lost default")
		}
		added, err := manager.Register(ctx, &Auth{ID: "new", Provider: "codex"})
		if err != nil {
			t.Fatal(err)
		}
		if added.EffectiveWebsocketsEnabled() != enabled {
			t.Fatal("new auth did not inherit default")
		}
	}
}
