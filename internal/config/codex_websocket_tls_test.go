package config

import "testing"

func TestCodexWebsocketTLSConfig(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte("config-version: 8\noauth:\n  providers:\n    codex:\n      websocket-tls-profile: codex\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Codex.WebsocketTLSProfile != "codex" {
		t.Fatal("profile not loaded")
	}
	if cfg.ForAPIKey().Codex.WebsocketTLSProfile != "" {
		t.Fatal("OAuth profile leaked into API key config")
	}
}

func TestCodexWebsocketTLSConfigRejectsUnknown(t *testing.T) {
	if _, err := ParseConfigBytes([]byte("config-version: 8\noauth:\n  providers:\n    codex:\n      websocket-tls-profile: typo\n")); err == nil {
		t.Fatal("unknown TLS profile accepted")
	}
}
