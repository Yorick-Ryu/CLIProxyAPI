package diff

import (
	"slices"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestCodexTimezoneChangeReported(t *testing.T) {
	oldCfg := &config.Config{}
	newCfg := &config.Config{Codex: config.CodexConfig{Timezone: config.CodexTimezoneConfig{
		Mode: "fixed", Zone: "Asia/Tokyo",
	}}}
	if !slices.Contains(BuildConfigChangeDetails(oldCfg, newCfg), "codex.timezone: updated") {
		t.Fatal("timezone changes must be reported")
	}
	if slices.Contains(BuildConfigChangeDetails(newCfg, newCfg), "codex.timezone: updated") {
		t.Fatal("unchanged timezone policy reported as changed")
	}
}
