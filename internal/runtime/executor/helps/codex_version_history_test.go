package helps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func testCodexHistory() []string {
	return []string{"0.161.0", "0.162.0", "0.162.1", "0.163.0", "0.164.0"}
}

func TestCodexHistoryStartupCheckReturnsWithoutPeriodicPolling(t *testing.T) {
	for _, offline := range []bool{false, true} {
		t.Run(fmt.Sprintf("offline=%t", offline), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var calls atomic.Int32
			store := &codexVersionStore{generation: 1, lifetime: ctx}
			store.fetchHistory = func(context.Context, string) ([]string, error) {
				calls.Add(1)
				if offline {
					return nil, errors.New("offline")
				}
				return testCodexHistory(), nil
			}
			done := make(chan struct{})
			go func() {
				store.runHistory(ctx, 1, "", "")
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("history check kept running instead of exiting after startup discovery")
			}
			cfg := &config.Config{}
			cfg.CodexHeaderDefaults.AccountUserAgent.Enabled = true
			store.start(ctx, cfg)
			store.start(ctx, cfg)
			if calls.Load() != 1 {
				t.Fatal("startup or config reload repeated release discovery")
			}
		})
	}
}

func TestCodexHistoryDiscoversVerifiedStableReleasesAcrossPages(t *testing.T) {
	type tag struct {
		Name string `json:"name"`
	}
	first := []tag{{"rust-v0.165.0"}, {"rust-v0.164.0"}, {"rust-v0.163.0"}, {"rust-v0.164.2"}, {"rust-v0.164.1"}}
	for len(first) < 100 {
		first = append(first, tag{fmt.Sprintf("rust-v0.165.0-alpha.%d", len(first))})
	}
	pages := 0
	releases := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("User-Agent") != "CLIProxyAPI-Codex-Version-Sync" {
			t.Error("unexpected release lookup headers")
		}
		if r.URL.Path == "/tags" {
			pages++
			if r.URL.Query().Get("per_page") != "100" {
				t.Error("unexpected page size")
			}
			if r.URL.Query().Get("page") == "1" {
				_ = json.NewEncoder(w).Encode(first)
			} else {
				_ = json.NewEncoder(w).Encode([]tag{{"rust-v0.161.0"}, {"rust-v0.162.0"}, {"rust-v0.162.1"}, {"rust-v0.163.0"}})
			}
			return
		}
		releases++
		name := strings.TrimPrefix(r.URL.Path, "/releases/tags/")
		if strings.Contains(name, "alpha") {
			t.Error("queried a prerelease tag")
		}
		if name == "rust-v0.164.1" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": name, "draft": name == "rust-v0.165.0", "prerelease": name == "rust-v0.164.2"})
	}))
	defer srv.Close()
	got, err := fetchCodexStableHistoryFrom(context.Background(), srv.Client(), srv.URL)
	if err != nil || !reflect.DeepEqual(got, testCodexHistory()) || pages != 2 || releases != 8 {
		t.Fatalf("history=%v err=%v pages=%d releases=%d", got, err, pages, releases)
	}
}

func TestCodexHistoryRejectsIncompleteAndFailedDiscovery(t *testing.T) {
	for _, mode := range []string{"incomplete", "rate-limit", "invalid-json", "mismatched-tag"} {
		t.Run(mode, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "rate-limit" {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				if mode == "invalid-json" {
					_, _ = w.Write([]byte(`{`))
					return
				}
				if r.URL.Path == "/tags" {
					_, _ = w.Write([]byte(`[{"name":"rust-v0.164.0"}]`))
					return
				}
				tag := "rust-v0.164.0"
				if mode == "mismatched-tag" {
					tag = "rust-v99.0.0"
				}
				_ = json.NewEncoder(w).Encode(map[string]string{"tag_name": tag})
			}))
			defer srv.Close()
			if _, err := fetchCodexStableHistoryFrom(context.Background(), srv.Client(), srv.URL); err == nil {
				t.Fatal("accepted an unverified or incomplete release history")
			}
		})
	}
}

func TestCodexAccountUserAgentsAdvanceEverySlotWithoutChangingPlatform(t *testing.T) {
	old := testCodexHistory()
	next := []string{"0.162.0", "0.162.1", "0.163.0", "0.164.0", "0.165.0"}
	slots, systems, terminals := map[int]bool{}, map[string]bool{}, map[string]bool{}
	for i := 0; i < 500; i++ {
		identity := fmt.Sprintf("account:synthetic-%d", i)
		ua, version := CodexAccountUserAgent(identity, old)
		slot := slices.Index(old, version)
		if slot < 0 {
			t.Fatal("assigned a version outside the five-release window")
		}
		slots[slot] = true
		nextUA, nextVersion := CodexAccountUserAgent(identity, next)
		if nextVersion != next[slot] || strings.ReplaceAll(ua, version, "VERSION") != strings.ReplaceAll(nextUA, nextVersion, "VERSION") {
			t.Fatalf("profile moved instead of its version slot: %q -> %q", ua, nextUA)
		}
		if repeat, _ := CodexAccountUserAgent(identity, slices.Clone(old)); repeat != ua {
			t.Fatal("same account changed identity without a history update")
		}
		if !strings.HasPrefix(ua, "codex-tui/"+version+" (") || !strings.HasSuffix(ua, " (codex-tui; "+version+")") || !strings.Contains(ua, "; x86_64) ") {
			t.Fatalf("inconsistent Linux UA: %q", ua)
		}
		for _, system := range codexLinuxSystems {
			if strings.Contains(ua, "("+system.token()+";") {
				systems[system.token()] = true
			}
		}
		for _, terminal := range codexLinuxTerminals {
			if strings.Contains(ua, ") "+terminal.token()+" (") {
				terminals[terminal.token()] = true
			}
		}
	}
	if len(slots) != 5 || len(systems) != len(codexLinuxSystems) || len(terminals) != len(codexLinuxTerminals) {
		t.Fatal("account assignment did not distribute across profiles and all five version slots")
	}
	for _, versions := range [][]string{nil, old[:4], {"0.161.0", "0.162.0", "0.162.1", "0.163.0", "0.164.0-alpha.1"}, {"0.161.0", "0.162.0", "0.162.1", "0.164.0", "0.163.0"}} {
		if ua, _ := CodexAccountUserAgent("account:test", versions); ua != "" {
			t.Fatal("assigned a profile before a complete valid history existed")
		}
	}
	if ua, _ := CodexAccountUserAgent("", old); ua != "" {
		t.Fatal("assigned an anonymous shared profile")
	}
}

func TestCodexHistoryRefreshIsDurableMonotonicAndIgnoresFailures(t *testing.T) {
	path := codexVersionHistoryPath(t.TempDir())
	if strings.HasSuffix(path, ".json") {
		t.Fatal("history would be ingested by the auth watcher")
	}
	old := testCodexHistory()
	store := &codexVersionStore{generation: 7}
	refresh := func(ctx context.Context, generation uint64, versions []string, err error) {
		store.refreshHistory(ctx, generation, "", path, func(context.Context, string) ([]string, error) { return versions, err })
	}
	refresh(context.Background(), 7, old, nil)
	got, err := loadCodexVersionHistory(path)
	if err != nil || !reflect.DeepEqual(got, old) || !reflect.DeepEqual(store.history, old) {
		t.Fatalf("failed to commit initial cache: %v, %v", got, err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("cache permissions are not private")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	next := []string{"0.162.0", "0.162.1", "0.163.0", "0.164.0", "0.165.0"}
	refresh(context.Background(), 7, nil, errors.New("offline"))
	refresh(context.Background(), 7, next[:4], nil)
	refresh(context.Background(), 6, next, nil)
	refresh(canceled, 7, next, nil)
	if !reflect.DeepEqual(store.history, old) {
		t.Fatal("failed, incomplete, canceled or obsolete discovery changed identities")
	}
	// A stale server omits 0.162.1. Merge known history rather than moving an
	// account to an older version, even though the newest release advances.
	partial := []string{"0.159.0", "0.160.0", "0.163.0", "0.164.0", "0.165.0"}
	refresh(context.Background(), 7, partial, nil)
	refresh(context.Background(), 7, old, nil)
	got, err = loadCodexVersionHistory(path)
	if err != nil || !reflect.DeepEqual(store.history, next) || !reflect.DeepEqual(got, next) {
		t.Fatalf("history regressed or runtime/cache differ: %v %v %v", store.history, got, err)
	}
	if extra, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".codex-version-history-*")); len(extra) != 0 {
		t.Fatal("temporary cache files were retained")
	}
	store.refreshHistory(context.Background(), 7, "", filepath.Join(path, "unwritable"), func(context.Context, string) ([]string, error) {
		return []string{"0.162.1", "0.163.0", "0.164.0", "0.165.0", "0.166.0"}, nil
	})
	if !reflect.DeepEqual(store.history, next) {
		t.Fatal("changed runtime history despite a failed cache commit")
	}
}

func TestCodexHistoryRestartUsesCacheWhenOffline(t *testing.T) {
	path := codexVersionHistoryPath(t.TempDir())
	if err := saveCodexVersionHistory(path, testCodexHistory()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &codexVersionStore{}
	called := make(chan struct{})
	store.fetchHistory = func(context.Context, string) ([]string, error) {
		cancel()
		close(called)
		return nil, errors.New("offline")
	}
	cfg := &config.Config{AuthDir: filepath.Dir(path)}
	cfg.CodexHeaderDefaults.AccountUserAgent.Enabled = true
	store.start(ctx, cfg)
	// A request immediately after startup must see the cached slots, before
	// background discovery has finished (or even begun).
	store.mu.Lock()
	loaded := slices.Clone(store.history)
	store.mu.Unlock()
	if !reflect.DeepEqual(loaded, testCodexHistory()) {
		t.Fatal("restart lost the last verified account version slots")
	}
	<-called
	if err := os.WriteFile(path, []byte(`["0.164.0\r\nInjected: header"]`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCodexVersionHistory(path); err == nil {
		t.Fatal("accepted corrupt history")
	}
}
