package config

import "testing"

func TestCodexAnalyticsConfigScopeAndMigration(t *testing.T) {
	for _, raw := range []string{
		"config-version: 8\noauth: {providers: {codex: {}}}\n",
		"config-version: 8\noauth: {providers: {codex: {analytics_enabled: null}}}\n",
	} {
		cfg, err := ParseConfigBytes([]byte(raw))
		if err != nil || cfg.Codex.AnalyticsEnabled != nil {
			t.Fatalf("default must preserve analytics: %v", err)
		}
	}
	for _, value := range []string{"false", "true"} {
		legacy := []byte("codex: {analytics_enabled: " + value + "}\n")
		migrated, _, err := NormalizeConfigLayout(legacy, true)
		if err != nil {
			t.Fatal(err)
		}
		if err := ValidateV8Config(migrated); err != nil {
			t.Fatal(err)
		}
		cfg, err := ParseConfigBytes(migrated)
		if err != nil || cfg.Codex.AnalyticsEnabled == nil || *cfg.Codex.AnalyticsEnabled != (value == "true") {
			t.Fatalf("migration lost explicit analytics policy %s: %v", value, err)
		}
		if cfg.ForAPIKey().Codex.AnalyticsEnabled != nil {
			t.Fatal("OAuth-only policy leaked into API-key upstream settings")
		}
		if cfg.Codex.AnalyticsEnabled == nil || *cfg.Codex.AnalyticsEnabled != (value == "true") {
			t.Fatal("API-key scoping changed shared configuration")
		}
	}
}
