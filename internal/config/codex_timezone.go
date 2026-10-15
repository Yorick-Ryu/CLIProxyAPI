package config

import (
	"fmt"
	"strings"
	_ "time/tzdata"
)

// CodexTimezonePolicy controls generation of the current environment date and timezone.
type CodexTimezonePolicy struct {
	Mode string `yaml:"mode" json:"mode"`
}

// CodexTimezoneConfig supplies the default for credentials without an override.
type CodexTimezoneConfig = CodexTimezonePolicy

// CodexTimezoneFromMetadata reads the credential-owned mode. A nil policy inherits.
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
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	return &policy, nil
}

func (c CodexTimezonePolicy) Validate() error {
	switch c.Mode {
	case "", "off", "local", "egress":
		return nil
	default:
		return fmt.Errorf("codex.timezone.mode must be off, local, or egress")
	}
}
