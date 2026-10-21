package config

import "testing"

func TestCodexVersionSyncV8Config(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte("config-version: 8\noauth:\n  providers:\n    codex:\n      header-defaults:\n        version-sync: {enabled: true}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.CodexHeaderDefaults.VersionSync.Enabled {
		t.Fatal("version sync config not loaded")
	}
	if cfg.ForAPIKey().CodexHeaderDefaults.VersionSync.Enabled {
		t.Fatal("OAuth sync leaked to API-key config")
	}
}
