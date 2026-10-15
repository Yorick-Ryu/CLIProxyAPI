package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCodexTimezoneValidation(t *testing.T) {
	for _, mode := range []string{"", "off", "local", "egress"} {
		if err := (CodexTimezoneConfig{Mode: mode}).Validate(); err != nil {
			t.Fatal(err)
		}
	}
	for _, mode := range []string{"fixed", "bad", "LOCAL"} {
		if (CodexTimezoneConfig{Mode: mode}).Validate() == nil {
			t.Fatalf("accepted mode %q", mode)
		}
	}
}

func TestCodexTimezoneLoadsFromYAML(t *testing.T) {
	for _, mode := range []string{"off", "local", "egress"} {
		for _, layout := range []struct{ prefix, suffix string }{
			{"codex: {timezone: {mode: ", "}}\n"},
			{"config-version: 8\noauth: {providers: {codex: {timezone: {mode: ", "}}}}\n"},
		} {
			raw := layout.prefix + mode + layout.suffix
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadConfig(path)
			if err != nil {
				t.Fatal(raw, err)
			}
			if cfg.Codex.Timezone.Mode != mode {
				t.Fatal("timezone mode lost")
			}
		}
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("codex: {timezone: {mode: fixed, zone: America/Los_Angeles}}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("retired fixed mode must require an explicit migration")
	}
}

func TestCodexTimezoneAccountMetadata(t *testing.T) {
	for _, mode := range []string{"off", "local", "egress"} {
		policy, err := CodexTimezoneFromMetadata(map[string]any{"codex_timezone_mode": mode})
		if err != nil || policy == nil || policy.Mode != mode {
			t.Fatalf("mode %s: %v", mode, err)
		}
	}
	for _, metadata := range []map[string]any{nil, {}, {"codex_timezone_mode": "inherit"}, {"codex_timezone_mode": nil}} {
		policy, err := CodexTimezoneFromMetadata(metadata)
		if policy != nil || err != nil {
			t.Fatal("expected inheritance")
		}
	}
	for _, metadata := range []map[string]any{{"codex_timezone_mode": true}, {"codex_timezone_mode": "bad"}, {"codex_timezone_mode": "fixed"}} {
		if _, err := CodexTimezoneFromMetadata(metadata); err == nil {
			t.Fatal("accepted invalid metadata")
		}
	}
}
