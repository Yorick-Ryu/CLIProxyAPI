package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCodexTimezoneValidation(t *testing.T) {
	for _, c := range []CodexTimezoneConfig{{}, {Mode: "off"}, {Mode: "egress"}, {Mode: "fixed", Zone: "America/Los_Angeles"}} {
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []CodexTimezoneConfig{{Mode: "bad"}, {Mode: "fixed"}, {Mode: "fixed", Zone: "Local"}, {Mode: "fixed", Zone: "bad/zone"}} {
		if c.Validate() == nil {
			t.Fatalf("accepted %#v", c)
		}
	}
}

func TestCodexTimezoneLoadsFromYAML(t *testing.T) {
	for _, mode := range []string{"off", "fixed", "egress"} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte("codex:\n  timezone:\n    mode: "+mode+"\n    zone: America/Los_Angeles\n"), 0600); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Codex.Timezone.Mode != mode || cfg.Codex.Timezone.Zone != "America/Los_Angeles" {
			t.Fatal("timezone config lost")
		}
	}
}

func TestCodexTimezoneAccountMetadata(t *testing.T) {
	for _, mode := range []string{"off", "fixed", "egress"} {
		policy, err := CodexTimezoneFromMetadata(map[string]any{"codex_timezone_mode": mode, "codex_timezone": "Asia/Tokyo"})
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
	for _, metadata := range []map[string]any{{"codex_timezone_mode": true}, {"codex_timezone_mode": "bad"}, {"codex_timezone_mode": "fixed"}, {"codex_timezone_mode": "fixed", "codex_timezone": "Local"}} {
		if _, err := CodexTimezoneFromMetadata(metadata); err == nil {
			t.Fatal("accepted invalid metadata")
		}
	}
}
