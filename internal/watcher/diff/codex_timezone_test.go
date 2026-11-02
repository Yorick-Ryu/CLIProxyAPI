package diff

import (
	"slices"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestCodexRequestLocationChangeReported(t *testing.T) {
	oldCfg := &config.Config{}
	newCfg := &config.Config{Codex: config.CodexConfig{RequestLocation: config.CodexRequestLocationConfig{
		Enabled:              true,
		CodexRequestLocation: config.CodexRequestLocation{Country: "US", Region: "Ohio", City: "Piketon", Timezone: "America/New_York"},
	}}}
	if !slices.Contains(BuildConfigChangeDetails(oldCfg, newCfg), "codex.request-location: updated") {
		t.Fatal("request location changes must be reported")
	}
	if slices.Contains(BuildConfigChangeDetails(newCfg, newCfg), "codex.request-location: updated") {
		t.Fatal("unchanged request location reported as changed")
	}
}
