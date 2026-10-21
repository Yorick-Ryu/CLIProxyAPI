package config

import (
	"fmt"
	"strings"
)

func (c *CodexConfig) normalizeWebsocketTLSProfile() error {
	c.WebsocketTLSProfile = strings.ToLower(strings.TrimSpace(c.WebsocketTLSProfile))
	switch c.WebsocketTLSProfile {
	case "", "native", "codex":
		return nil
	default:
		return fmt.Errorf("codex.websocket-tls-profile must be native or codex")
	}
}

func (c *CodexConfig) normalizeHTTPTLSProfile() error {
	c.HTTPTLSProfile = strings.ToLower(strings.TrimSpace(c.HTTPTLSProfile))
	switch c.HTTPTLSProfile {
	case "", "chrome", "linux":
		return nil
	default:
		return fmt.Errorf("codex.http-tls-profile must be chrome or linux")
	}
}
