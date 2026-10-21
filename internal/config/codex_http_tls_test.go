package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCodexHTTPTLSConfig(t *testing.T) {
	for _, mode := range []string{"chrome", "linux", "typo"} {
		data := []byte("config-version: 8\noauth:\n  providers:\n    codex:\n      http-tls-profile: " + mode + "\n")
		p := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(p, data, 0600); err != nil {
			t.Fatal(err)
		}
		for _, load := range []func() (*Config, error){func() (*Config, error) { return ParseConfigBytes(data) }, func() (*Config, error) { return LoadConfig(p) }} {
			cfg, err := load()
			if mode == "typo" {
				if err == nil {
					t.Fatal("unknown profile accepted")
				}
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Codex.HTTPTLSProfile != mode {
				t.Fatal("profile not loaded")
			}
			if cfg.ForAPIKey().Codex.HTTPTLSProfile != "" {
				t.Fatal("OAuth profile leaked into API-key config")
			}
		}
	}
}
