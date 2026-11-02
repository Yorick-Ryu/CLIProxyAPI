package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validRequestLocation() CodexRequestLocation {
	return CodexRequestLocation{Country: "US", Region: "Ohio", City: "Piketon", Timezone: "America/New_York"}
}

func TestCodexRequestLocationValidation(t *testing.T) {
	if err := (CodexRequestLocationConfig{}).Validate(); err != nil {
		t.Fatal("disabled empty location must be valid")
	}
	if err := (CodexRequestLocationConfig{Enabled: true, CodexRequestLocation: validRequestLocation()}).Validate(); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*CodexRequestLocation){
		func(l *CodexRequestLocation) { l.Country = "us" },
		func(l *CodexRequestLocation) { l.Country = "USA" },
		func(l *CodexRequestLocation) { l.Region = " " },
		func(l *CodexRequestLocation) { l.City = "bad\ncity" },
		func(l *CodexRequestLocation) { l.City = strings.Repeat("x", 129) },
		func(l *CodexRequestLocation) { l.Timezone = "Mars/Base" },
		func(l *CodexRequestLocation) { l.Timezone = "Local" },
	} {
		location := validRequestLocation()
		mutate(&location)
		if (CodexRequestLocationConfig{Enabled: true, CodexRequestLocation: location}).Validate() == nil {
			t.Fatalf("accepted invalid location %+v", location)
		}
	}
}

func TestCodexRequestLocationLoadsFromYAML(t *testing.T) {
	const location = "{enabled: true, country: US, region: ' Ohio ', city: Piketon, timezone: America/New_York}"
	for _, raw := range []string{
		"codex: {request-location: " + location + "}\n",
		"config-version: 8\noauth: {providers: {codex: {request-location: " + location + "}}}\n",
	} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatal(raw, err)
		}
		if !cfg.Codex.RequestLocation.Enabled || cfg.Codex.RequestLocation.CodexRequestLocation != validRequestLocation() {
			t.Fatalf("request location lost: %+v", cfg.Codex.RequestLocation)
		}
		parsed, err := ParseConfigBytes([]byte(raw))
		if err != nil || parsed.Codex.RequestLocation.CodexRequestLocation != validRequestLocation() {
			t.Fatalf("parsed request location: %+v %v", parsed, err)
		}
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := []byte("codex: {request-location: {enabled: true, country: US, timezone: Bad/Zone}}\n")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("LoadConfig accepted invalid enabled location")
	}
	if _, err := ParseConfigBytes(raw); err == nil {
		t.Fatal("ParseConfigBytes accepted invalid enabled location")
	}
}

func TestCodexRequestLocationAccountMetadata(t *testing.T) {
	for _, metadata := range []map[string]any{nil, {}, {"codex_request_location_mode": nil}, {"codex_request_location_mode": ""}, {"codex_request_location_mode": "inherit"}} {
		policy, err := CodexRequestLocationFromMetadata(metadata)
		if err != nil || policy.Mode != "inherit" || policy.Location != nil {
			t.Fatalf("expected inheritance: %+v %v", policy, err)
		}
	}
	for _, mode := range []string{"off", "on", "egress"} {
		policy, err := CodexRequestLocationFromMetadata(map[string]any{"codex_request_location_mode": mode})
		if err != nil || policy.Mode != mode {
			t.Fatalf("mode %s: %+v %v", mode, policy, err)
		}
	}
	policy, err := CodexRequestLocationFromMetadata(map[string]any{"codex_request_location": map[string]any{"country": "US", "region": " Ohio ", "city": "Piketon", "timezone": "America/New_York"}})
	if err != nil || policy.Location == nil || *policy.Location != validRequestLocation() {
		t.Fatalf("account location: %+v %v", policy, err)
	}
	for _, metadata := range []map[string]any{
		{"codex_request_location_mode": true},
		{"codex_request_location_mode": "local"},
		{"codex_request_location_mode": "fixed"},
		{"codex_request_location": "US"},
		{"codex_request_location": map[string]any{"timezone": "America/New_York"}},
		{"codex_request_location": map[string]any{"country": "US", "region": "Ohio", "city": "Piketon", "timezone": 1}},
	} {
		if _, err := CodexRequestLocationFromMetadata(metadata); err == nil {
			t.Fatalf("accepted invalid metadata %v", metadata)
		}
	}
}
