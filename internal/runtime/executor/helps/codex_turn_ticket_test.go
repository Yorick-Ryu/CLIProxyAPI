package helps

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	auth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func ticketFixture() (*config.Config, *auth.Auth, string) {
	cfg := &config.Config{}
	cfg.Codex.TurnStateTicket = config.CodexTurnStateTicketConfig{Enabled: true, Models: []string{"gpt-5.6-luna"}}
	a := &auth.Auth{ID: "a", Provider: "codex", Metadata: map[string]any{"access_token": "test-token", "account_id": "account-a"}}
	return cfg, a, "gAAAAA" + strings.Repeat("A", 286)
}

func TestCodexTurnTicketIdentityExpiryAndFailOpen(t *testing.T) {
	cfg, a, value := ticketFixture()
	s := NewCodexTurnTicketStore()
	now := time.Unix(100, 0)
	s.now = func() time.Time { return now }
	if !s.Capture(cfg, a, "gpt-5.6-luna", 200, value) {
		t.Fatal("capture failed")
	}
	for _, tt := range []struct {
		name   string
		change func(*config.Config, *auth.Auth)
		model  string
	}{
		{"other account", func(_ *config.Config, a *auth.Auth) { a.ID = "b" }, "gpt-5.6-luna"},
		{"replaced identity", func(_ *config.Config, a *auth.Auth) { a.Metadata["account_id"] = "b" }, "gpt-5.6-luna"},
		{"other model", func(*config.Config, *auth.Auth) {}, "gpt-5.6-terra"},
		{"disabled", func(c *config.Config, _ *auth.Auth) { c.Codex.TurnStateTicket.Enabled = false }, "gpt-5.6-luna"},
		{"disabled account", func(_ *config.Config, a *auth.Auth) { a.Disabled = true }, "gpt-5.6-luna"},
		{"API key", func(_ *config.Config, a *auth.Auth) { a.Attributes = map[string]string{"api_key": "test-api-key"} }, "gpt-5.6-luna"},
		{"custom upstream", func(_ *config.Config, a *auth.Auth) {
			a.Attributes = map[string]string{"base_url": "https://example.com"}
		}, "gpt-5.6-luna"},
		{"allowlist", func(c *config.Config, _ *auth.Auth) { c.Codex.TurnStateTicket.AccountIDs = []string{"b"} }, "gpt-5.6-luna"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, a, _ := ticketFixture()
			tt.change(c, a)
			h := http.Header{codexTicketHeader: []string{"client-state"}}
			if s.Apply(c, a, tt.model, h) || h.Get(codexTicketHeader) != "client-state" {
				t.Fatal("cross-scope injection or changed fail-open header")
			}
		})
	}
	h := http.Header{"x-codex-turn-state": []string{"lowercase-client-state"}, codexTicketHeader: []string{"client-state"}}
	if !s.Apply(cfg, a, "gpt-5.6-luna", h) || h.Get(codexTicketHeader) != value {
		t.Fatal("ticket not applied")
	}
	if len(h) != 1 {
		t.Fatal("duplicate state headers survived replacement")
	}
	now = now.Add(50 * time.Minute)
	if !s.needsRefresh(cfg, a, "gpt-5.6-luna") {
		t.Fatal("refresh threshold missed")
	}
	if s.Capture(cfg, a, "gpt-5.6-luna", 503, value) {
		t.Fatal("error accepted")
	}
	if !s.Apply(cfg, a, "gpt-5.6-luna", http.Header{}) {
		t.Fatal("failed refresh discarded valid ticket")
	}
	now = now.Add(10 * time.Minute)
	if s.Apply(cfg, a, "gpt-5.6-luna", http.Header{}) {
		t.Fatal("expired ticket applied")
	}
}

func TestCodexTurnTicketValidationRedactionAndConcurrency(t *testing.T) {
	cfg, a, value := ticketFixture()
	s := NewCodexTurnTicketStore()
	for _, bad := range []string{"", strings.Repeat("X", 292), value + "A", value[:291], value[:291] + "\n"} {
		if s.Capture(cfg, a, "gpt-5.6-luna", 200, bad) {
			t.Fatal("invalid shape accepted")
		}
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Capture(cfg, a, "gpt-5.6-luna", 200, value)
			s.Apply(cfg, a, "gpt-5.6-luna", http.Header{})
			s.Snapshot()
		}()
	}
	wg.Wait()
	b, _ := json.Marshal(s.Snapshot())
	if strings.Contains(string(b), value) || strings.Contains(string(b), "test-token") {
		t.Fatal("secret in status")
	}
	if got := util.MaskSensitiveHeaderValue("x-Codex-Turn-State", value); strings.Contains(got, value[:10]) {
		t.Fatal("ticket logged")
	}
	cfg.Codex.TurnStateTicket.HarvestProxyURL = "http://user:secret@localhost:8888"
	b, _ = json.Marshal(cfg)
	if strings.Contains(string(b), "user:secret") {
		t.Fatal("proxy exposed through management JSON")
	}
}

func TestCodexTurnTicketTransportAndCancellation(t *testing.T) {
	for _, url := range []string{"", "direct", "socks5://127.0.0.1:9", "http://127.0.0.1:9"} {
		tr, err := ticketTransport(url)
		if err != nil {
			t.Fatal(err)
		}
		if !tr.DisableKeepAlives || tr.ForceAttemptHTTP2 || tr.TLSNextProto == nil {
			t.Fatal("harvest transport reused connections")
		}
		tr.CloseIdleConnections()
	}
	if _, err := ticketTransport("invalid://user:secret@example.com"); err == nil {
		t.Fatal("invalid proxy accepted")
	}
	cfg, a, _ := ticketFixture()
	a.ProxyURL = "socks5://127.0.0.1:9"
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := NewCodexTurnTicketStore()
	if r := s.Probe(ctx, cfg, a, "gpt-5.6-luna"); r.Captured || r.Error != "transport" {
		t.Fatalf("cancelled probe: %+v", r)
	}
	s.Run(ctx, func() *config.Config { return cfg }, func() []*auth.Auth { return []*auth.Auth{a} })
}
