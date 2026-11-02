package config

import (
	"fmt"
	"strings"
	"time"
	_ "time/tzdata"
	"unicode"
	"unicode/utf8"
)

// CodexRequestLocation is the location sent in supported Codex request fields.
type CodexRequestLocation struct {
	Country  string `yaml:"country" json:"country"`
	Region   string `yaml:"region" json:"region"`
	City     string `yaml:"city" json:"city"`
	Timezone string `yaml:"timezone" json:"timezone"`
}

// CodexRequestLocationConfig is the global request-location setting.
type CodexRequestLocationConfig struct {
	Enabled              bool `yaml:"enabled" json:"enabled"`
	CodexRequestLocation `yaml:",inline" json:",inline"`
}

// CodexRequestLocationPolicy is an account's request-location setting.
type CodexRequestLocationPolicy struct {
	Mode     string
	Location *CodexRequestLocation
}

// Validate rejects incomplete global locations when the override is enabled.
func (c CodexRequestLocationConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	if err := c.CodexRequestLocation.Validate(); err != nil {
		return fmt.Errorf("codex.request-location: %w", err)
	}
	return nil
}

// Validate checks the fields required by time-context and Web Search overrides.
func (l CodexRequestLocation) Validate() error {
	if len(l.Country) != 2 || l.Country[0] < 'A' || l.Country[0] > 'Z' || l.Country[1] < 'A' || l.Country[1] > 'Z' {
		return fmt.Errorf("country must be a two-letter uppercase ISO code")
	}
	for name, value := range map[string]string{"region": l.Region, "city": l.City} {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" || utf8.RuneCountInString(trimmed) > 128 || strings.IndexFunc(value, unicode.IsControl) >= 0 {
			return fmt.Errorf("%s must contain 1-128 non-control characters", name)
		}
	}
	zone := strings.TrimSpace(l.Timezone)
	if zone == "" || zone == "Local" {
		return fmt.Errorf("timezone must be a valid IANA timezone")
	}
	if _, err := time.LoadLocation(zone); err != nil {
		return fmt.Errorf("timezone must be a valid IANA timezone")
	}
	return nil
}

// Normalize trims text fields after validation.
func (l CodexRequestLocation) Normalize() CodexRequestLocation {
	l.Country = strings.TrimSpace(l.Country)
	l.Region = strings.TrimSpace(l.Region)
	l.City = strings.TrimSpace(l.City)
	l.Timezone = strings.TrimSpace(l.Timezone)
	return l
}

// CodexRequestLocationFromMetadata reads account mode and account/proxy location.
func CodexRequestLocationFromMetadata(metadata map[string]any) (CodexRequestLocationPolicy, error) {
	policy := CodexRequestLocationPolicy{Mode: "inherit"}
	if raw, exists := metadata["codex_request_location_mode"]; exists && raw != nil {
		mode, ok := raw.(string)
		if !ok {
			return policy, fmt.Errorf("codex_request_location_mode must be a string")
		}
		mode = strings.TrimSpace(mode)
		switch mode {
		case "", "inherit", "off", "on", "egress":
			if mode != "" {
				policy.Mode = mode
			}
		default:
			return policy, fmt.Errorf("codex_request_location_mode must be inherit, off, on, or egress")
		}
	}
	if raw, exists := metadata["codex_request_location"]; exists && raw != nil {
		encoded, ok := raw.(map[string]any)
		if !ok {
			return policy, fmt.Errorf("codex_request_location must be an object")
		}
		read := func(name string) (string, error) {
			value, ok := encoded[name].(string)
			if !ok {
				return "", fmt.Errorf("codex_request_location.%s must be a string", name)
			}
			return value, nil
		}
		var location CodexRequestLocation
		var err error
		if location.Country, err = read("country"); err != nil {
			return policy, err
		}
		if location.Region, err = read("region"); err != nil {
			return policy, err
		}
		if location.City, err = read("city"); err != nil {
			return policy, err
		}
		if location.Timezone, err = read("timezone"); err != nil {
			return policy, err
		}
		if err = location.Validate(); err != nil {
			return policy, fmt.Errorf("codex_request_location: %w", err)
		}
		location = location.Normalize()
		policy.Location = &location
	}
	return policy, nil
}
