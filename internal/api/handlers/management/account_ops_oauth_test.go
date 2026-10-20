package management

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestAccountOpsOAuthCommitGuards(t *testing.T) {
	for _, mode := range []string{"preserve", "configuration changed", "cancelled", "workspace mismatch", "expired"} {
		t.Run(mode, func(t *testing.T) {
			metadata := map[string]any{"type": "codex", "email": "fixture@example.com", "account_id": "team-fixture", "plan_type": "team", "access_token": "old-fixture-token", "priority": 10, "websockets": false, "disable_cooling": false, "note": "到期时间：2099-01-01 00:00（北京时间）"}
			path := filepath.Join(t.TempDir(), "codex-fixture.json")
			write := func() {
				data, _ := json.Marshal(metadata)
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			write()
			guard := &accountOpsOAuthGuard{Name: "codex-fixture.json", Email: "fixture@example.com", Workspace: "team-fixture", Plan: "team", Version: accountOpsConfigVersion(metadata)}
			state := "ops-guard-" + mode
			RegisterOAuthSession(state, "codex")
			defer CompleteOAuthSession(state)
			storage := &codex.CodexTokenStorage{Email: "fixture@example.com", AccountID: "team-fixture", PlanType: "team", AccessToken: "new-fixture-token", ReplaceCredentials: true}
			record := &coreauth.Auth{ID: "new-file.json", FileName: "new-file.json", Provider: "codex", Storage: storage, Metadata: map[string]any{"email": "fixture@example.com", "account_id": "team-fixture", "plan_type": "team"}}
			if mode == "workspace mismatch" {
				storage.AccountID = "personal-fixture"
			}
			errBind := guard.bind(state, record)
			if mode == "workspace mismatch" {
				if errBind == nil {
					t.Fatal("wrong workspace was accepted")
				}
				return
			}
			if errBind != nil {
				t.Fatal(errBind)
			}
			if record.FileName != "codex-fixture.json" {
				t.Fatal("original credential path not retained")
			}
			switch mode {
			case "configuration changed":
				metadata["websockets"] = true
				write()
			case "cancelled":
				CompleteOAuthSession(state)
			case "expired":
				metadata["note"] = "到期时间：2020-01-01 00:00（北京时间）"
				guard.Version = accountOpsConfigVersion(metadata)
				guard.Automatic = true
				write()
			}
			errSave := storage.SaveTokenToFile(path)
			if mode == "preserve" && errSave != nil {
				t.Fatal(errSave)
			}
			if mode != "preserve" && errSave == nil {
				t.Fatal("unsafe commit was accepted")
			}
			raw, _ := os.ReadFile(path)
			var after map[string]any
			_ = json.Unmarshal(raw, &after)
			expectedToken := "old-fixture-token"
			if mode == "preserve" {
				expectedToken = "new-fixture-token"
			}
			if after["access_token"] != expectedToken {
				t.Fatal("wrong token persisted")
			}
			if after["disable_cooling"] != false || after["priority"] != float64(10) {
				t.Fatal("account settings were not retained")
			}
		})
	}
}

func TestAccountOpsNewLoginRejectsFreeAndExistingFile(t *testing.T) {
	state := "ops-new-login-fixture"
	RegisterOAuthSession(state, "codex")
	defer CompleteOAuthSession(state)
	guard := &accountOpsOAuthGuard{Email: "fixture@example.com"}
	storage := &codex.CodexTokenStorage{Email: guard.Email, AccountID: "team-fixture", PlanType: "free", AccessToken: "new-fixture-token", ReplaceCredentials: true}
	record := &coreauth.Auth{Provider: "codex", Storage: storage}
	if guard.bind(state, record) == nil {
		t.Fatal("Free fallback was accepted")
	}
	storage.PlanType = "team"
	if err := guard.bind(state, record); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "already-exists.json")
	if err := os.WriteFile(path, []byte(`{"type":"codex","access_token":"old-fixture-token"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if storage.SaveTokenToFile(path) == nil {
		t.Fatal("existing credential overwritten by new login")
	}
}

func TestAccountOpsTermBoundaries(t *testing.T) {
	now := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		note  string
		valid bool
	}{
		{"到期时间：2026-10-20 18:01（北京时间）", true},
		{"到期时间：2026-10-20 18:00（北京时间）", false},
		{"", false},
		{"到期时间：2026-10-21 18:00 UTC", false},
		{"到期时间：2026-10-21 18:00\n到期时间：2026-10-22 18:00", false},
	} {
		if accountOpsTermValid(map[string]any{"note": tc.note, "plan_type": "team"}, now) != tc.valid {
			t.Errorf("unexpected validity for %q", tc.note)
		}
	}
}
