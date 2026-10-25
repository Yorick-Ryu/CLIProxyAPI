package helps

import (
	"fmt"
	"strings"
	"testing"
)

func TestCodexLinuxProfilesPreserveDistributionAndTerminalVersionPairs(t *testing.T) {
	versions := testCodexHistory()
	archGhostty, genericGhostty, wezterm, konsole, vscode := false, false, false, false, false
	for i := 0; i < 2000; i++ {
		ua, version := CodexAccountUserAgent(fmt.Sprintf("account:profile-fixture-%d", i), versions)
		if strings.Contains(ua, "ghostty/1.3.1-arch2") {
			archGhostty = true
			if !strings.Contains(ua, "(Arch Linux Rolling Release; x86_64)") {
				t.Fatalf("distribution-specific terminal used on another system: %q", ua)
			}
			// A Codex release update must not rewrite the terminal/package version.
			next, _ := CodexAccountUserAgent(fmt.Sprintf("account:profile-fixture-%d", i), []string{"0.162.0", "0.162.1", "0.163.0", "0.164.0", "0.165.0"})
			if !strings.Contains(next, "ghostty/1.3.1-arch2") || next == ua {
				t.Fatalf("terminal version changed with the Codex release: %q -> %q", ua, next)
			}
		}
		genericGhostty = genericGhostty || strings.Contains(ua, ") Ghostty (")
		wezterm = wezterm || strings.Contains(ua, ") WezTerm (")
		konsole = konsole || strings.Contains(ua, ") Konsole (")
		vscode = vscode || strings.Contains(ua, ") vscode (")
		if strings.ContainsAny(ua, "\r\n\x00") || strings.Contains(ua, "Rolling Release.0") || strings.Contains(ua, ") vscode/"+version+" (") {
			t.Fatalf("profile coerced a system or terminal version: %q", ua)
		}
		if !strings.HasPrefix(ua, "codex-tui/"+version+" ") || !strings.HasSuffix(ua, "(codex-tui; "+version+")") {
			t.Fatalf("terminal identity replaced the TUI client identity: %q", ua)
		}
	}
	if !archGhostty || !genericGhostty || !wezterm || !konsole || !vscode {
		t.Fatal("new terminal profiles are unreachable")
	}
}
