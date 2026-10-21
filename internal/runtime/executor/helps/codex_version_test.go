package helps

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestCodexStableReleaseValidation(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{`{"tag_name":"rust-v0.163.0","draft":false,"prerelease":false}`, "0.163.0"},
		{`{"tag_name":"rust-v0.164.0-alpha.1"}`, ""},
		{`{"tag_name":"rust-v0.163.0","prerelease":true}`, ""},
		{`{"tag_name":"rust-v0.163.0","draft":true}`, ""},
		{`{"tag_name":"rust-v0.163.0\r\ninjected"}`, ""},
		{`{`, ""},
	} {
		got, err := decodeCodexStableVersion(strings.NewReader(tc.raw))
		if got != tc.want || (err != nil) != (tc.want == "") {
			t.Fatalf("got %q, %v for %s", got, err, tc.raw)
		}
	}
}

func TestCodexVersionStartupOnly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	called := make(chan struct{}, 2)
	store := &codexVersionStore{fetch: func(context.Context, string) (string, error) { called <- struct{}{}; return "0.163.0", nil }}
	cfg := &config.Config{}
	store.start(ctx, cfg)
	cfg.CodexHeaderDefaults.VersionSync.Enabled = true
	store.start(ctx, cfg)
	select {
	case <-called:
		t.Fatal("disabled startup or reload initiated discovery")
	default:
	}
	cancel()
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	store.start(ctx2, cfg)
	<-called
	store.start(ctx2, cfg)
	select {
	case <-called:
		t.Fatal("duplicate startup check")
	default:
	}
}

func TestCodexVersionCheckSuccessFailureAndCancellation(t *testing.T) {
	store := &codexVersionStore{generation: 1}
	ctx := context.Background()
	store.check(ctx, 1, "", func(context.Context, string) (string, error) { return "0.163.0", nil })
	if store.version != "0.163.0" {
		t.Fatal("successful check not published")
	}
	store.check(ctx, 1, "", func(context.Context, string) (string, error) { return "", errors.New("offline") })
	if store.version != "0.163.0" {
		t.Fatal("failure changed snapshot")
	}
	stale, cancel := context.WithCancel(ctx)
	cancel()
	store.check(stale, 1, "", func(context.Context, string) (string, error) { return "9.9.9", nil })
	store.check(ctx, 0, "", func(context.Context, string) (string, error) { return "9.9.9", nil })
	if store.version != "0.163.0" {
		t.Fatal("obsolete check changed snapshot")
	}
}

func TestCodexVersionUserAgentOverridesOnlyClientVersion(t *testing.T) {
	ua := "codex_exec/0.162.0 (Mac OS 27.0.1; arm64) ghostty/1.3.1 (codex_exec; 0.162.0)"
	got := CodexVersionUserAgent(ua, "codex-tui/0.162.0", "0.163.0")
	want := "codex_exec/0.163.0 (Mac OS 27.0.1; arm64) ghostty/1.3.1 (codex_exec; 0.163.0)"
	if got != want {
		t.Fatalf("got %q", got)
	}
	if CodexVersionUserAgent("custom/1", "codex-tui/0.162.0", "0.163.0") != "codex-tui/0.163.0" {
		t.Fatal("unrecognized identity did not use fallback")
	}
}
