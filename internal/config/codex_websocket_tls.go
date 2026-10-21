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
