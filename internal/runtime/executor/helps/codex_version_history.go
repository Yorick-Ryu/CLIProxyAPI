package helps

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	log "github.com/sirupsen/logrus"
)

const (
	codexVersionHistorySize = 5
	codexReleasesAPI        = "https://api.github.com/repos/openai/codex"
)

// CodexVersionHistory returns an independent oldest-first snapshot. It is empty
// until a complete verified history has been loaded from cache or GitHub.
func CodexVersionHistory() []string {
	codexVersions.mu.Lock()
	defer codexVersions.mu.Unlock()
	return slices.Clone(codexVersions.history)
}

func codexVersionHistoryPath(authDir string) string {
	if strings.TrimSpace(authDir) == "" {
		return ""
	}
	// Deliberately not .json: the auth watcher must never ingest this cache.
	return filepath.Join(authDir, ".codex-version-history")
}

func (s *codexVersionStore) runHistory(ctx context.Context, generation uint64, proxy, cachePath string) {
	s.mu.Lock()
	fetch := s.fetchHistory
	s.mu.Unlock()
	if fetch == nil {
		fetch = fetchCodexStableHistory
	}
	s.refreshHistory(ctx, generation, proxy, cachePath, fetch)
}

func (s *codexVersionStore) refreshHistory(ctx context.Context, generation uint64, proxy, cachePath string, fetch func(context.Context, string) ([]string, error)) {
	if ctx.Err() != nil {
		return
	}
	versions, err := fetch(ctx, proxy)
	if err != nil || !validCodexVersionHistory(versions) {
		if ctx.Err() == nil {
			log.Warn("codex: release history check failed; keeping previous account identities")
		}
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx.Err() != nil || generation != s.generation {
		return
	}
	// Never roll back slots due to stale/partial upstream data or a late
	// backport. Only a release newer than the current newest rolls the window.
	if len(s.history) > 0 {
		if compareCodexVersions(versions[len(versions)-1], s.history[len(s.history)-1]) <= 0 {
			return
		}
		versions = newestCodexVersions(append(slices.Clone(s.history), versions...))
	}
	if errSave := saveCodexVersionHistory(cachePath, versions); errSave != nil {
		log.Warn("codex: could not persist version history; keeping previous account identities")
		return
	}
	s.history = slices.Clone(versions)
	log.WithField("versions", versions).Info("codex: account CLI version history updated")
}

func loadCodexVersionHistory(path string) ([]string, error) {
	if path == "" {
		return nil, os.ErrNotExist
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if errClose := f.Close(); errClose != nil {
			log.Warn("codex: could not close version history cache")
		}
	}()
	var versions []string
	if errDecode := json.NewDecoder(io.LimitReader(f, 4096)).Decode(&versions); errDecode != nil {
		return nil, errDecode
	}
	if !validCodexVersionHistory(versions) {
		return nil, fmt.Errorf("invalid version history")
	}
	return versions, nil
}

func saveCodexVersionHistory(path string, versions []string) error {
	if path == "" {
		return nil
	}
	data, err := json.Marshal(versions)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".codex-version-history-*")
	if err != nil {
		return err
	}
	defer func() {
		if errRemove := os.Remove(f.Name()); errRemove != nil && !os.IsNotExist(errRemove) {
			log.Warn("codex: could not remove temporary version history cache")
		}
	}()
	if _, errWrite := f.Write(data); errWrite != nil {
		_ = f.Close()
		return errWrite
	}
	if errSync := f.Sync(); errSync != nil {
		_ = f.Close()
		return errSync
	}
	if errClose := f.Close(); errClose != nil {
		return errClose
	}
	return os.Rename(f.Name(), path)
}

func validCodexVersionHistory(versions []string) bool {
	if len(versions) != codexVersionHistorySize {
		return false
	}
	for i, version := range versions {
		if !codexStableVersion.MatchString("rust-v" + version) {
			return false
		}
		for _, part := range strings.Split(version, ".") {
			if _, err := strconv.ParseUint(part, 10, 32); err != nil {
				return false
			}
		}
		if i > 0 && compareCodexVersions(versions[i-1], version) >= 0 {
			return false
		}
	}
	return true
}

func compareCodexVersions(a, b string) int {
	ap, bp := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < 3; i++ {
		av, _ := strconv.ParseUint(ap[i], 10, 32)
		bv, _ := strconv.ParseUint(bp[i], 10, 32)
		if av < bv {
			return -1
		}
		if av > bv {
			return 1
		}
	}
	return 0
}

func newestCodexVersions(versions []string) []string {
	slices.SortFunc(versions, compareCodexVersions)
	versions = slices.Compact(versions)
	if len(versions) > codexVersionHistorySize {
		versions = versions[len(versions)-codexVersionHistorySize:]
	}
	return versions
}

func fetchCodexStableHistory(ctx context.Context, proxy string) ([]string, error) {
	cfg := &config.Config{}
	cfg.ProxyURL = proxy
	client := NewProxyAwareHTTPClient(ctx, cfg, nil, 0)
	defer client.CloseIdleConnections()
	return fetchCodexStableHistoryFrom(ctx, client, codexReleasesAPI)
}

func fetchCodexStableHistoryFrom(ctx context.Context, client *http.Client, api string) ([]string, error) {
	var versions []string
	checked := make(map[string]bool)
	// Tags omit the large asset lists. Confirm each numeric tag has an actual
	// non-draft, non-prerelease release before admitting it to the window.
	for page := 1; page <= 10; page++ {
		var tags []struct {
			Name string `json:"name"`
		}
		if _, err := codexHistoryGET(ctx, client, fmt.Sprintf("%s/tags?per_page=100&page=%d", api, page), &tags); err != nil {
			return nil, err
		}
		var candidates []string
		for _, tag := range tags {
			if codexStableVersion.MatchString(tag.Name) && !checked[tag.Name] {
				checked[tag.Name] = true
				candidates = append(candidates, strings.TrimPrefix(tag.Name, "rust-v"))
			}
		}
		slices.SortFunc(candidates, func(a, b string) int { return compareCodexVersions(b, a) })
		for _, candidate := range candidates {
			var release struct {
				Tag        string `json:"tag_name"`
				Draft      bool   `json:"draft"`
				Prerelease bool   `json:"prerelease"`
			}
			status, err := codexHistoryGET(ctx, client, api+"/releases/tags/rust-v"+candidate, &release)
			if status == http.StatusNotFound {
				continue
			}
			if err != nil {
				return nil, err
			}
			if release.Draft || release.Prerelease || release.Tag != "rust-v"+candidate {
				continue
			}
			versions = append(versions, candidate)
			if len(versions) == codexVersionHistorySize {
				versions = newestCodexVersions(versions)
				if !validCodexVersionHistory(versions) {
					return nil, fmt.Errorf("invalid release versions")
				}
				return versions, nil
			}
		}
		if len(tags) < 100 {
			break
		}
	}
	return nil, fmt.Errorf("not enough stable Codex releases")
}

func codexHistoryGET(ctx context.Context, client *http.Client, url string, target any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "CLIProxyAPI-Codex-Version-Sync")
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Warn("codex: could not close release history response")
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, fmt.Errorf("release history status %d", resp.StatusCode)
	}
	return resp.StatusCode, json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(target)
}
