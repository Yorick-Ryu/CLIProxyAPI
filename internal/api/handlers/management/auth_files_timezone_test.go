package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	fileauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestPatchAuthFileTimezoneSettings(t *testing.T) {
	authDir := t.TempDir()
	store := fileauth.NewFileTokenStore()
	store.SetBaseDir(authDir)
	manager := coreauth.NewManager(store, nil, nil)
	_, err := manager.Register(context.Background(), &coreauth.Auth{ID: "account.json", FileName: "account.json", Provider: "codex", Attributes: map[string]string{"path": filepath.Join(authDir, "account.json")}, Metadata: map[string]any{"type": "codex", "untouched": "keep"}})
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	patch := func(fields string, want int) {
		t.Helper()
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/auth-files/fields", strings.NewReader(`{"name":"account.json",`+fields+`}`))
		h.PatchAuthFileFields(ctx)
		if rec.Code != want {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
	}
	patch(`"codex_timezone_mode":"fixed","codex_timezone":"Asia/Tokyo"`, 200)
	get := func() *coreauth.Auth {
		a, ok := manager.GetByID("account.json")
		if !ok {
			t.Fatal("missing auth")
		}
		return a
	}
	if get().Metadata["codex_timezone"] != "Asia/Tokyo" {
		t.Fatal("not saved")
	}
	data, err := os.ReadFile(filepath.Join(authDir, "account.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]any
	if err = json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if saved["codex_timezone_mode"] != "fixed" || saved["codex_timezone"] != "Asia/Tokyo" {
		t.Fatal("settings not persisted")
	}
	entry := h.buildAuthFileEntry(get())
	if entry["codex_timezone_mode"] != "fixed" || entry["codex_timezone"] != "Asia/Tokyo" {
		t.Fatal("account list omits timezone")
	}
	patch(`"codex_timezone_mode":"fixed","codex_timezone":"invalid/zone"`, 400)
	if get().Metadata["codex_timezone"] != "Asia/Tokyo" {
		t.Fatal("invalid patch changed live metadata")
	}
	patch(`"codex_timezone_mode.nested":"off"`, 400)
	patch(`"codex_timezone_mode":"off"`, 200)
	if get().Metadata["codex_timezone_mode"] != "off" {
		t.Fatal("off not saved")
	}
	patch(`"codex_timezone_mode":"egress"`, 200)
	patch(`"codex_timezone_mode":null,"codex_timezone":null`, 200)
	if policy, err := config.CodexTimezoneFromMetadata(get().Metadata); policy != nil || err != nil {
		t.Fatal("inherit failed")
	}
	if get().Metadata["untouched"] != "keep" {
		t.Fatal("unrelated field changed")
	}
}
