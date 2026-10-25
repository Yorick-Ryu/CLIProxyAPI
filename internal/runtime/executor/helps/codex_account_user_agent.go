package helps

import (
	"crypto/sha256"
	"fmt"
)

type codexLinuxSystem struct {
	Name    string
	Version string
}

func (s codexLinuxSystem) token() string { return s.Name + " " + s.Version }

type codexLinuxTerminal struct {
	Name    string
	Version string
	// A distribution-specific package version must not appear on other systems.
	System string
}

func (t codexLinuxTerminal) token() string {
	if t.Version != "" {
		return t.Name + "/" + t.Version
	}
	return t.Name
}

// These ordered tables are part of the unreleased profile v1. System/version
// and terminal/version are indivisible pairs, not arbitrary version numbers.
// Once released, changing table order, length or eligibility reassigns accounts;
// keep these tables fixed across automatic Codex release-history updates.
var codexLinuxSystems = [...]codexLinuxSystem{
	{Name: "Ubuntu", Version: "20.4.0"},
	{Name: "Ubuntu", Version: "22.4.0"},
	{Name: "Ubuntu", Version: "24.4.0"},
	{Name: "Debian", Version: "12.0.0"},
	{Name: "Debian", Version: "13.0.0"},
	{Name: "Ubuntu", Version: "26.4.0"},
	{Name: "Arch Linux", Version: "Rolling Release"},
}

var codexLinuxTerminals = [...]codexLinuxTerminal{
	{Name: "xterm-256color"},
	{Name: "kitty"},
	{Name: "Alacritty"},
	{Name: "gnome-terminal"},
	{Name: "Ghostty"},
	{Name: "WezTerm"},
	{Name: "Konsole"},
	{Name: "vscode"},
	// Captured in the app-server initialize response reported in Codex #23572.
	{Name: "ghostty", Version: "1.3.1-arch2", System: "Arch Linux"},
}

func codexAccountLinuxProfile(seed [32]byte) (codexLinuxSystem, codexLinuxTerminal) {
	system := codexLinuxSystems[int(seed[1])%len(codexLinuxSystems)]
	var eligible [len(codexLinuxTerminals)]codexLinuxTerminal
	n := 0
	for _, terminal := range codexLinuxTerminals {
		if terminal.System == "" || terminal.System == system.Name {
			eligible[n] = terminal
			n++
		}
	}
	return system, eligible[int(seed[2])%n]
}

// CodexAccountUserAgent deterministically distributes stable account identities
// over Linux profiles and five age-ranked version slots. No tokens, request IDs,
// filenames (when an account identity exists), clocks or mutable RNG are used.
// Slots are oldest first: every new release advances all accounts, including
// accounts that were already on the newest version. Platform fields stay fixed.
func CodexAccountUserAgent(identity string, versions []string) (string, string) {
	if identity == "" || !validCodexVersionHistory(versions) {
		return "", ""
	}
	seed := sha256.Sum256([]byte("cli-proxy-api:codex-linux-user-agent:v1:" + identity))
	version := versions[int(seed[0])%codexVersionHistorySize]
	system, terminal := codexAccountLinuxProfile(seed)
	return fmt.Sprintf("codex-tui/%s (%s; x86_64) %s (codex-tui; %s)", version, system.token(), terminal.token(), version), version
}
