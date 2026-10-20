package auth

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
)

func TestCredentialCommitGuardDoesNotPublishFailedConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "codex-guard.json")
	manager := NewManager(&simpleCodexDiskStore{path: path}, nil, nil)
	record := &Auth{ID: "codex-guard.json", FileName: "codex-guard.json", Provider: "codex", Metadata: map[string]any{"type": "codex", "access_token": "synthetic-old", "priority": float64(2), "websockets": false}}
	if _, err := manager.Register(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	disk := map[string]any{"type": "codex", "access_token": "synthetic-rotated", "priority": float64(12), "websockets": false}
	if err := codex.WriteJSONFileAtomic(path, disk); err != nil {
		t.Fatal(err)
	}
	desired, ok := manager.GetByID(record.ID)
	if !ok {
		t.Fatal("missing runtime credential")
	}
	desired.Metadata["priority"] = float64(10)
	conflict := errors.New("synthetic configuration conflict")
	ctx := WithCredentialCommitGuard(context.Background(), func(current map[string]any) error {
		if current["priority"] != float64(2) {
			return conflict
		}
		return nil
	})
	if _, err := manager.Update(ctx, desired); !errors.Is(err, conflict) {
		t.Fatalf("guard error=%v", err)
	}
	actual, _ := manager.GetByID(record.ID)
	if actual.Metadata["priority"] != float64(2) {
		t.Fatal("failed write changed the runtime credential")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var persisted map[string]any
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted["priority"] != float64(12) || persisted["access_token"] != "synthetic-rotated" {
		t.Fatal("conflicting disk state was overwritten")
	}
}

func TestCredentialCommitGuardRetainsConcurrentTokenRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "codex-token-rotation.json")
	if err := codex.WriteJSONFileAtomic(path, map[string]any{"type": "codex", "access_token": "synthetic-fresh", "refresh_token": "synthetic-refresh", "websockets": false}); err != nil {
		t.Fatal(err)
	}
	record := &Auth{Provider: "codex", Metadata: map[string]any{"type": "codex", "access_token": "synthetic-stale", "websockets": true}}
	ctx := WithCredentialCommitGuard(context.Background(), func(current map[string]any) error {
		if current["websockets"] != false {
			return errors.New("synthetic conflict")
		}
		return nil
	})
	if _, err := PersistCodexAuthFile(ctx, path, record); err != nil {
		t.Fatal(err)
	}
	if record.Metadata["access_token"] != "synthetic-fresh" || record.Metadata["websockets"] != true {
		t.Fatal("configuration write lost the concurrent token rotation")
	}
}
