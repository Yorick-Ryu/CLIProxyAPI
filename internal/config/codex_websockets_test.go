package config

import (
	"encoding/json"
	"gopkg.in/yaml.v3"
	"testing"
)

func TestCodexWebsocketsDefault(t *testing.T) {
	for _, tc := range []struct {
		name, document string
		want           bool
	}{
		{"omitted", "{}", true},
		{"empty codex", "codex: {}", true},
		{"enabled", "codex: {websockets-default: true}", true},
		{"disabled", "codex: {websockets-default: false}", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var cfg Config
			if err := yaml.Unmarshal([]byte(tc.document), &cfg); err != nil {
				t.Fatal(err)
			}
			if got := cfg.CodexWebsocketsDefault(); got != tc.want {
				t.Fatalf("default = %t, want %t", got, tc.want)
			}
			clone := cfg.CloneForRuntime()
			data, err := json.Marshal(clone)
			if err != nil {
				t.Fatal(err)
			}
			var roundTrip Config
			if err := json.Unmarshal(data, &roundTrip); err != nil {
				t.Fatal(err)
			}
			if roundTrip.CodexWebsocketsDefault() != tc.want {
				t.Fatal("JSON round trip lost default")
			}
		})
	}
}

func TestCodexWebsocketMessageLimitRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		document string
		want     int64
	}{
		{"{}", 0},
		{"codex: {websocket-max-message-bytes: 0}", 0},
		{"codex: {websocket-max-message-bytes: 20000000}", 20_000_000},
		{"codex: {websocket-max-message-bytes: 30000000}", 30_000_000},
	} {
		var cfg Config
		if err := yaml.Unmarshal([]byte(tc.document), &cfg); err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(cfg.CloneForRuntime())
		if err != nil {
			t.Fatal(err)
		}
		var roundTrip Config
		if err := json.Unmarshal(data, &roundTrip); err != nil {
			t.Fatal(err)
		}
		if roundTrip.Codex.WebsocketMaxMessageBytes != tc.want {
			t.Fatalf("lost limit: got %d, want %d", roundTrip.Codex.WebsocketMaxMessageBytes, tc.want)
		}
	}
}

func TestCodexKeyWebsocketsTriState(t *testing.T) {
	var cfg Config
	if err := yaml.Unmarshal([]byte("codex-api-key:\n  - api-key: omitted\n  - api-key: disabled\n    websockets: false\n  - api-key: enabled\n    websockets: true\n"), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.CodexKey[0].Websockets != nil || cfg.CodexKey[1].Websockets == nil || *cfg.CodexKey[1].Websockets || cfg.CodexKey[2].Websockets == nil || !*cfg.CodexKey[2].Websockets {
		t.Fatal("lost omitted/false/true distinction")
	}
	data, err := yaml.Marshal(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	var restored Config
	if err := yaml.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.CodexKey[1].Websockets == nil || *restored.CodexKey[1].Websockets {
		t.Fatal("explicit false lost on save")
	}
}
