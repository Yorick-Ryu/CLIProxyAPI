package auth

import (
	"context"
	"testing"
	"testing/synctest"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestCodexWebsocketsReloadDuringPersistence(t *testing.T) {
	for _, operation := range []string{"register", "update"} {
		t.Run(operation, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx := context.Background()
				store := &blockingEnrichingAuthStore{entered: make(chan struct{}), release: make(chan struct{})}
				manager := NewManager(store, nil, nil)
				manager.SetConfig(&config.Config{})
				input := &Auth{ID: "ws-persistence", Provider: "codex", Status: StatusActive, Metadata: map[string]any{"type": "codex"}}
				if operation == "update" {
					var err error
					input, err = manager.Register(WithSkipPersist(ctx), input)
					if err != nil {
						t.Fatal(err)
					}
				}
				done := make(chan error, 1)
				go func() {
					var err error
					if operation == "register" {
						_, err = manager.Register(ctx, input)
					} else {
						_, err = manager.Update(ctx, input)
					}
					done <- err
				}()
				<-store.entered
				disabled := false
				manager.SetConfig(&config.Config{Codex: config.CodexConfig{WebsocketsDefault: &disabled}})
				close(store.release)
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				published, ok := manager.GetByID(input.ID)
				if !ok || published.EffectiveWebsocketsEnabled() {
					t.Fatal("publication restored the stale WebSocket default")
				}
				manager.scheduler.mu.Lock()
				scheduled := manager.scheduler.providers["codex"].auths[input.ID].auth.Clone()
				manager.scheduler.mu.Unlock()
				if scheduled.EffectiveWebsocketsEnabled() {
					t.Fatal("scheduler retained the stale WebSocket default")
				}
				if _, exists := published.Metadata["websockets"]; exists {
					t.Fatal("inherited default became a persistent override")
				}
			})
		})
	}
}
