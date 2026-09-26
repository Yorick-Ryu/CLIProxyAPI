package config

import (
	"fmt"
	"strings"
	"time"
	_ "time/tzdata"
)

// CodexTimezonePolicy controls optional Codex environment rewriting.
type CodexTimezonePolicy struct {
	Mode string `yaml:"mode" json:"mode"`
	Zone string `yaml:"zone" json:"zone"`
}

// CodexTimezoneConfig supplies the default for credentials without an override.
type CodexTimezoneConfig = CodexTimezonePolicy

// CodexTimezoneFromMetadata reads flat, credential-owned settings. A nil policy inherits.
func CodexTimezoneFromMetadata(metadata map[string]any) (*CodexTimezonePolicy, error) {
	raw, exists := metadata["codex_timezone_mode"]
	if !exists || raw == nil {
		return nil, nil
	}
	mode, ok := raw.(string)
	if !ok {
		return nil, fmt.Errorf("codex_timezone_mode must be a string")
	}
	mode = strings.TrimSpace(mode)
	if mode == "" || mode == "inherit" {
		return nil, nil
	}
	policy := CodexTimezonePolicy{Mode: mode}
	if rawZone := metadata["codex_timezone"]; rawZone != nil {
		zone, ok := rawZone.(string)
		if !ok {
			return nil, fmt.Errorf("codex_timezone must be an IANA timezone string")
		}
		policy.Zone = strings.TrimSpace(zone)
	}
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	return &policy, nil
}

// Validate rejects ambiguous modes and invalid fixed IANA zones.
func (c CodexTimezonePolicy) Validate() error {
	switch c.Mode {
	case "", "off", "egress":
		return nil
	case "fixed":
		if c.Zone == "" || c.Zone == "Local" {
			return fmt.Errorf("codex.timezone.zone must be an explicit IANA timezone")
		}
		if _, err := time.LoadLocation(c.Zone); err != nil {
			return fmt.Errorf("codex.timezone.zone is not a valid IANA timezone")
		}
		return nil
	default:
		return fmt.Errorf("codex.timezone.mode must be off, fixed, or egress")
	}
}
