package helps

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	log "github.com/sirupsen/logrus"
)

const codexReleaseURL = "https://api.github.com/repos/openai/codex/releases/latest"

var codexStableVersion = regexp.MustCompile(`^rust-v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
var codexUAVersion = regexp.MustCompile(`(codex[-_a-zA-Z0-9]*)(/|; )[^ ;()]+`)
var codexVersions = &codexVersionStore{}

type codexVersionStore struct {
	mu           sync.Mutex
	lifetime     context.Context
	generation   uint64
	version      string
	fetch        func(context.Context, string) (string, error)
	history      []string
	fetchHistory func(context.Context, string) ([]string, error)
}

// StartCodexVersionUpdater checks once at service startup, including in
// per-account history mode. Reloads and requests never trigger release discovery.
func StartCodexVersionUpdater(ctx context.Context, cfg *config.Config) { codexVersions.start(ctx, cfg) }

// CodexCurrentVersion is empty until this startup's discovery succeeds.
func CodexCurrentVersion() string {
	codexVersions.mu.Lock()
	defer codexVersions.mu.Unlock()
	return codexVersions.version
}

// CodexVersionUserAgent preserves platform and terminal fields. Unrecognized
// custom agents use the built-in identity so the enabled override is effective.
func CodexVersionUserAgent(ua, fallback, version string) string {
	if !codexUAVersion.MatchString(ua) {
		ua = fallback
	}
	return codexUAVersion.ReplaceAllString(ua, "${1}${2}"+version)
}

func (s *codexVersionStore) start(ctx context.Context, cfg *config.Config) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx == nil || ctx.Err() != nil || (s.lifetime != nil && s.lifetime.Err() == nil) {
		return
	}
	s.lifetime = ctx
	s.generation++
	s.version = ""
	s.history = nil
	if cfg == nil {
		return
	}
	if cfg.CodexHeaderDefaults.AccountUserAgent.Enabled {
		path := codexVersionHistoryPath(cfg.AuthDir)
		if versions, err := loadCodexVersionHistory(path); err == nil {
			s.history = versions
		}
		go s.runHistory(ctx, s.generation, cfg.ProxyURL, path)
		return
	}
	if !cfg.CodexHeaderDefaults.VersionSync.Enabled {
		return
	}
	fetch := s.fetch
	if fetch == nil {
		fetch = fetchCodexStableVersion
	}
	go s.check(ctx, s.generation, cfg.ProxyURL, fetch)
}
func (s *codexVersionStore) check(ctx context.Context, generation uint64, proxy string, fetch func(context.Context, string) (string, error)) {
	version, err := fetch(ctx, proxy)
	s.mu.Lock()
	current := ctx.Err() == nil && generation == s.generation
	if current && err == nil {
		s.version = version
	}
	s.mu.Unlock()
	if !current {
		return
	}
	if err != nil {
		log.Warn("codex: startup CLI version check failed; keeping configured identity")
	} else {
		log.WithField("version", version).Info("codex: startup CLI version synchronized")
	}
}
func fetchCodexStableVersion(ctx context.Context, proxy string) (string, error) {
	cfg := &config.Config{}
	cfg.ProxyURL = proxy
	client := NewProxyAwareHTTPClient(ctx, cfg, nil, 0)
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, codexReleaseURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "CLIProxyAPI-Codex-Version-Sync")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("release status %d", resp.StatusCode)
	}
	return decodeCodexStableVersion(io.LimitReader(resp.Body, 1<<20))
}
func decodeCodexStableVersion(reader io.Reader) (string, error) {
	var release struct {
		Tag        string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	if err := json.NewDecoder(reader).Decode(&release); err != nil {
		return "", err
	}
	if release.Draft || release.Prerelease || !codexStableVersion.MatchString(release.Tag) {
		return "", fmt.Errorf("not a stable Codex release")
	}
	return strings.TrimPrefix(release.Tag, "rust-v"), nil
}
