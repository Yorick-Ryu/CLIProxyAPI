package helps

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	auth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/proxy"
)

const codexTicketHeader = "X-Codex-Turn-State"
const codexTicketURL = "https://chatgpt.com/backend-api/codex/responses"

type codexTurnTicket struct {
	value   string
	expires time.Time
}

// CodexTurnTicketStore isolates opaque state by credential identity and model.
type CodexTurnTicketStore struct {
	mu           sync.Mutex
	tickets      map[string]codexTurnTicket
	now          func() time.Time
	observations map[string]CodexTicketStatus
}

// CodexTicketStatus is safe for the authenticated management API.
type CodexTicketStatus struct {
	Account         string                 `json:"account"`
	Model           string                 `json:"model"`
	LastProbe       time.Time              `json:"last_probe"`
	Probe           CodexTicketProbeResult `json:"probe"`
	Ready           bool                   `json:"ready"`
	Expires         time.Time              `json:"expires_at,omitempty"`
	HeadersPrepared uint64                 `json:"headers_prepared"`
}

var DefaultCodexTurnTickets = NewCodexTurnTicketStore()

func NewCodexTurnTicketStore() *CodexTurnTicketStore {
	return &CodexTurnTicketStore{tickets: make(map[string]codexTurnTicket), observations: make(map[string]CodexTicketStatus), now: time.Now}
}

func codexTicketIdentity(a *auth.Auth, model string) string {
	accountID, _ := a.Metadata["account_id"].(string)
	sum := sha256.Sum256([]byte(a.ID + "\x00" + accountID + "\x00" + strings.TrimSpace(model)))
	return hex.EncodeToString(sum[:])
}

func ticketConfig(cfg *config.Config) config.CodexTurnStateTicketConfig {
	var c config.CodexTurnStateTicketConfig
	if cfg != nil {
		c = cfg.Codex.TurnStateTicket
	}
	if len(c.Models) == 0 {
		c.Models = []string{"gpt-6-astra", "gpt-5.6-sol"}
	}
	if c.TTLSeconds <= 0 || c.TTLSeconds > 3600 {
		c.TTLSeconds = 3600
	}
	if c.RefreshBeforeSeconds <= 0 || c.RefreshBeforeSeconds >= c.TTLSeconds {
		c.RefreshBeforeSeconds = c.TTLSeconds / 6
	}
	if c.ProbeIntervalSeconds < 6 {
		c.ProbeIntervalSeconds = 60
	}
	if c.MaxConcurrent <= 0 || c.MaxConcurrent > 8 {
		c.MaxConcurrent = 2
	}
	return c
}

func codexTicketEligible(c config.CodexTurnStateTicketConfig, a *auth.Auth, model string) bool {
	if !c.Enabled || a == nil || a.ID == "" || a.Provider != "codex" || a.Disabled || a.Status == auth.StatusDisabled {
		return false
	}
	if a.Attributes["api_key"] != "" || a.AuthKind() == auth.AuthKindAPIKey {
		return false
	}
	if base := strings.TrimRight(a.Attributes["base_url"], "/"); base != "" && base != "https://chatgpt.com/backend-api/codex" {
		return false
	}
	token, _ := a.Metadata["access_token"].(string)
	if strings.TrimSpace(token) == "" {
		return false
	}
	if len(c.AccountIDs) > 0 && !slices.Contains(c.AccountIDs, a.ID) {
		return false
	}
	return slices.Contains(c.Models, strings.TrimSpace(model))
}

func validCodexTurnTicket(value string) bool {
	if len(value) != 292 || !strings.HasPrefix(value, "gAAAAA") {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '=') {
			return false
		}
	}
	return true
}

// Capture validates upstream status and shape; TTL is a local policy, not a provider guarantee.
func (s *CodexTurnTicketStore) Capture(cfg *config.Config, a *auth.Auth, model string, status int, value string) bool {
	c := ticketConfig(cfg)
	if !codexTicketEligible(c, a, model) || status != http.StatusOK || !validCodexTurnTicket(value) {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for key, t := range s.tickets {
		if !now.Before(t.expires) {
			delete(s.tickets, key)
		}
	}
	key := codexTicketIdentity(a, model)
	if _, exists := s.tickets[key]; !exists && len(s.tickets) >= 4096 {
		return false
	}
	s.tickets[key] = codexTurnTicket{value: value, expires: now.Add(time.Duration(c.TTLSeconds) * time.Second)}
	return true
}

// Apply never acquires credentials on a user request and never rejects a missing ticket.
func (s *CodexTurnTicketStore) Apply(cfg *config.Config, a *auth.Auth, model string, h http.Header) bool {
	if h == nil || !codexTicketEligible(ticketConfig(cfg), a, model) {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := codexTicketIdentity(a, model)
	t, ok := s.tickets[key]
	if !ok || !s.now().Before(t.expires) {
		delete(s.tickets, key)
		return false
	}
	for name := range h {
		if strings.EqualFold(name, codexTicketHeader) {
			delete(h, name)
		}
	}
	h.Set(codexTicketHeader, t.value)
	status := s.observations[key]
	status.Account, status.Model = codexTicketIdentity(a, "")[:12], model
	status.HeadersPrepared++
	s.observations[key] = status
	return true
}

func (s *CodexTurnTicketStore) Snapshot() []CodexTicketStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]CodexTicketStatus, 0, len(s.observations))
	for key, status := range s.observations {
		if ticket, ok := s.tickets[key]; ok {
			status.Expires = ticket.expires
			status.Ready = s.now().Before(ticket.expires)
		}
		out = append(out, status)
	}
	return out
}

func (s *CodexTurnTicketStore) needsRefresh(cfg *config.Config, a *auth.Auth, model string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tickets[codexTicketIdentity(a, model)]
	return !ok || !t.expires.After(s.now().Add(time.Duration(ticketConfig(cfg).RefreshBeforeSeconds)*time.Second))
}

// ProbeResult intentionally exposes no credential, ticket, proxy URL or response body.
type CodexTicketProbeResult struct {
	Status   int    `json:"status"`
	Length   int    `json:"length"`
	Captured bool   `json:"captured"`
	Error    string `json:"error,omitempty"`
}

func ticketTransport(proxyURL string) (*http.Transport, error) {
	tr, mode, err := proxyutil.BuildHTTPTransport(proxyURL)
	if err != nil {
		return nil, errors.New("invalid harvest proxy")
	}
	if tr == nil || mode == proxyutil.ModeInherit {
		tr = proxyutil.NewDirectTransport()
	}
	setting, err := proxyutil.Parse(proxyURL)
	if err != nil {
		return nil, errors.New("invalid harvest proxy")
	}
	if setting.URL != nil && (setting.URL.Scheme == "socks5" || setting.URL.Scheme == "socks5h") {
		dialer, _, err := proxyutil.BuildDialer(proxyURL)
		if err != nil {
			return nil, errors.New("invalid harvest proxy")
		}
		cd, ok := dialer.(proxy.ContextDialer)
		if !ok {
			return nil, errors.New("harvest proxy lacks cancellation")
		}
		tr.DialContext = cd.DialContext
	} else if tr.DialContext == nil {
		tr.DialContext = (&net.Dialer{Timeout: 25 * time.Second}).DialContext
	}
	tr.DisableKeepAlives = true
	tr.ForceAttemptHTTP2 = false
	tr.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	if tr.TLSClientConfig == nil {
		tr.TLSClientConfig = &tls.Config{}
	} else {
		tr.TLSClientConfig = tr.TLSClientConfig.Clone()
	}
	tr.TLSClientConfig.NextProtos = []string{"http/1.1"}
	return tr, nil
}

// Probe performs one bounded credential-acquisition request over the selected proxy.
// It does not refresh OAuth tokens, update account health, or generate usage records.
func (s *CodexTurnTicketStore) Probe(ctx context.Context, cfg *config.Config, a *auth.Auth, model string) (result CodexTicketProbeResult) {
	c := ticketConfig(cfg)
	if !codexTicketEligible(c, a, model) {
		return CodexTicketProbeResult{Error: "ineligible"}
	}
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		key := codexTicketIdentity(a, model)
		if len(s.observations) >= 4096 {
			return
		}
		status := s.observations[key]
		status.Account, status.Model = codexTicketIdentity(a, "")[:12], model
		status.LastProbe, status.Probe = s.now(), result
		s.observations[key] = status
	}()
	proxyURL := c.HarvestProxyURL
	if proxyURL == "" {
		proxyURL = a.ProxyURL
	}
	if proxyURL == "" && cfg != nil {
		proxyURL = cfg.ProxyURL
	}
	tr, err := ticketTransport(proxyURL)
	if err != nil {
		return CodexTicketProbeResult{Error: "proxy_configuration"}
	}
	defer tr.CloseIdleConnections()
	body, _ := json.Marshal(map[string]any{"model": model, "store": false, "stream": true, "instructions": "Reply with exactly: pong", "input": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "ping"}}}}})
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, codexTicketURL, bytes.NewReader(body))
	if err != nil {
		return CodexTicketProbeResult{Error: "request"}
	}
	token, _ := a.Metadata["access_token"].(string)
	accountID, _ := a.Metadata["account_id"].(string)
	req.Close = true
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("ChatGPT-Account-ID", accountID)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("OpenAI-Beta", "responses=experimental")
	req.Header.Set("session_id", uuid.NewString())
	req.Header.Set("User-Agent", "codex_cli_rs/0.153.4 (Linux; x86_64)")
	req.Header.Set("Version", "0.153.4")
	req.Header.Set("Originator", "codex_cli_rs")
	client := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return CodexTicketProbeResult{Error: "transport"}
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			log.Debug("codex ticket probe body close failed")
		}
	}()
	value := strings.TrimSpace(resp.Header.Get(codexTicketHeader))
	result = CodexTicketProbeResult{Status: resp.StatusCode, Length: len(value)}
	result.Captured = s.Capture(cfg, a, model, resp.StatusCode, value)
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 8192))
	}
	return result
}

// Run owns a bounded background loop; cancellation stops all acquisition requests.
func (s *CodexTurnTicketStore) Run(ctx context.Context, getConfig func() *config.Config, listAccounts func() []*auth.Auth) {
	for ctx.Err() == nil {
		cfg := getConfig()
		c := ticketConfig(cfg)
		if c.Enabled {
			sem := make(chan struct{}, c.MaxConcurrent)
			var wg sync.WaitGroup
			for _, a := range listAccounts() {
				for _, model := range c.Models {
					if !codexTicketEligible(c, a, model) || !s.needsRefresh(cfg, a, model) {
						continue
					}
					select {
					case sem <- struct{}{}:
					case <-ctx.Done():
						wg.Wait()
						return
					}
					wg.Add(1)
					go func(a *auth.Auth, model string) {
						defer wg.Done()
						defer func() { <-sem }()
						result := s.Probe(ctx, cfg, a, model)
						log.WithFields(log.Fields{"account": codexTicketIdentity(a, "")[:12], "model": model, "status": result.Status, "length": result.Length, "captured": result.Captured, "error_kind": result.Error}).Info("codex ticket probe")
					}(a, model)
				}
			}
			wg.Wait()
		}
		timer := time.NewTimer(time.Duration(c.ProbeIntervalSeconds) * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
