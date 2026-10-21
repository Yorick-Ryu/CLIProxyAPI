package executor

import (
	"context"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"testing"
)

func TestCodexWebsocketTLSProfileDialer(t *testing.T) {
	for _, route := range []string{"", "direct", "http://127.0.0.1:18081", "socks5://127.0.0.1:18081"} {
		for _, profile := range []string{"", "native", "codex"} {
			cfg := &config.Config{}
			cfg.Codex.WebsocketTLSProfile = profile
			d := newProxyAwareWebsocketDialer(context.Background(), cfg, &cliproxyauth.Auth{ProxyURL: route})
			if (d.TLSHandshakeContext != nil) != (profile == "codex") {
				t.Fatalf("route %q profile %q: wrong hook", route, profile)
			}
			if (d.WriteHandshake != nil) != (profile == "codex") {
				t.Fatal("header profile not tied to TLS configuration")
			}
			if d.NetDialTLSContext != nil {
				t.Fatal("TLS dial bypasses proxy tunnel")
			}
			if !d.EnableCompression {
				t.Fatal("compression disabled")
			}
		}
	}
}
