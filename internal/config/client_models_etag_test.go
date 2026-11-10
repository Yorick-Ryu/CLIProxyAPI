package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestClientCodexStripModelsETag(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want bool
	}{
		{"config-version: 8\n", false},
		{"config-version: 8\nclient: {codex: {strip-models-etag: false}}", false},
		{"config-version: 8\nclient: {codex: {strip-models-etag: true}}", true},
	} {
		if err := ValidateV8Config([]byte(tc.raw)); err != nil {
			t.Fatal(err)
		}
		cfg, err := ParseConfigBytes([]byte(tc.raw))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Client.Codex.StripModelsETag != tc.want {
			t.Fatalf("unexpected flag for %s", tc.raw)
		}
		data, err := yaml.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		restored, err := ParseConfigBytes(data)
		if err != nil {
			t.Fatal(err)
		}
		if restored.Client.Codex.StripModelsETag != tc.want {
			t.Fatal("lost flag on config roundtrip")
		}
	}
	if err := ValidateV8Config([]byte("client: {codex: {strip-models-etag: invalid}}")); err == nil {
		t.Fatal("accepted non-boolean setting")
	}
}
