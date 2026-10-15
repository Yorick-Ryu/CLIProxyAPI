package diff

import (
	"slices"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestCodexAnalyticsChangeReported(t *testing.T) {
	for _, tc := range []struct {
		old, updated *bool
		want         string
	}{
		{nil, new(false), "inherit -> false"},
		{new(false), new(true), "false -> true"},
		{new(true), nil, "true -> inherit"},
	} {
		oldCfg := &config.Config{Codex: config.CodexConfig{AnalyticsEnabled: tc.old}}
		newCfg := &config.Config{Codex: config.CodexConfig{AnalyticsEnabled: tc.updated}}
		message := "codex.analytics_enabled: " + tc.want
		if !slices.Contains(BuildConfigChangeDetails(oldCfg, newCfg), message) {
			t.Fatal("analytics policy change must be reported", message)
		}
		if slices.Contains(BuildConfigChangeDetails(newCfg, newCfg), message) {
			t.Fatal("unchanged analytics policy reported as changed")
		}
	}
	a := &config.Config{Codex: config.CodexConfig{AnalyticsEnabled: new(false)}}
	b := &config.Config{Codex: config.CodexConfig{AnalyticsEnabled: new(false)}}
	if !optionalBoolEqual(a.Codex.AnalyticsEnabled, b.Codex.AnalyticsEnabled) {
		t.Fatal("equal values at different addresses must be equal")
	}
}
