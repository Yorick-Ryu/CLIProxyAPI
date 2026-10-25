package config

import "testing"

func TestCodexVersionSyncV8Config(t *testing.T) {
	for _, tc := range []struct {
		name, fields  string
		sync, account bool
	}{
		{"legacy", "version-sync: {enabled: true}", true, false},
		{"account-only", "account-user-agent: {enabled: true}", false, true},
		{"both", "version-sync: {enabled: true}\n        account-user-agent: {enabled: true}", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := ParseConfigBytes([]byte("config-version: 8\noauth:\n  providers:\n    codex:\n      header-defaults:\n        " + tc.fields + "\n"))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.CodexHeaderDefaults.VersionSync.Enabled != tc.sync || cfg.CodexHeaderDefaults.AccountUserAgent.Enabled != tc.account {
				t.Fatal("account UA and global version-sync switches are not independent")
			}
			if cfg.ForAPIKey().CodexHeaderDefaults.VersionSync.Enabled || cfg.ForAPIKey().CodexHeaderDefaults.AccountUserAgent.Enabled {
				t.Fatal("OAuth identity settings leaked to API-key config")
			}
		})
	}
}
