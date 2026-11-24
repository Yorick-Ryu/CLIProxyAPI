package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTransientErrorCooldownPolicyConfigParsing(t *testing.T) {
	t.Run("defaults respect retry-after with no cap", func(t *testing.T) {
		raw := []byte("config-version: 8\nrouting:\n  cooldown:\n    disable-cooling: false\n")
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatalf("write config: %v", err)
		}
		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig failed: %v", err)
		}
		if !cfg.RespectTransientErrorRetryAfter() {
			t.Fatalf("RespectTransientErrorRetryAfter() = false, want true")
		}
		if cfg.TransientErrorMaxCooldownSeconds != 0 {
			t.Fatalf("TransientErrorMaxCooldownSeconds = %d, want 0", cfg.TransientErrorMaxCooldownSeconds)
		}
	})

	t.Run("parses v8 routing.cooldown respect=false and max-cooldown-seconds", func(t *testing.T) {
		raw := []byte(`config-version: 8
routing:
  cooldown:
    disable-cooling: false
    transient-error-cooldown-seconds: 20
    transient-error-respect-retry-after: false
    transient-error-max-cooldown-seconds: 45
`)
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatalf("write config: %v", err)
		}
		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig failed: %v", err)
		}
		if cfg.TransientErrorCooldownSeconds != 20 {
			t.Fatalf("TransientErrorCooldownSeconds = %d, want 20", cfg.TransientErrorCooldownSeconds)
		}
		if cfg.RespectTransientErrorRetryAfter() {
			t.Fatalf("RespectTransientErrorRetryAfter() = true, want false")
		}
		if cfg.TransientErrorMaxCooldownSeconds != 45 {
			t.Fatalf("TransientErrorMaxCooldownSeconds = %d, want 45", cfg.TransientErrorMaxCooldownSeconds)
		}
	})
}

func TestTransientErrorCooldownPolicyConfigSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("config-version: 8\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	legacy := false
	for _, mode := range []*bool{&legacy, nil} {
		cfg.TransientErrorRespectRetryAfter = mode
		if err := SaveConfigPreserveComments(path, cfg); err != nil {
			t.Fatal(err)
		}
		reloaded, err := LoadConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := reloaded.RespectTransientErrorRetryAfter(), mode == nil; got != want {
			t.Fatalf("saved upstream mode = %t, want %t", got, want)
		}
	}
}
