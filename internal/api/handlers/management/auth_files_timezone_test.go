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
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	fileauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestPatchAuthFileRequestLocationSettings(t *testing.T) {
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
			t.Fatalf("%s: status %d: %s", fields, rec.Code, rec.Body.String())
		}
	}
	get := func() *coreauth.Auth {
		a, ok := manager.GetByID("account.json")
		if !ok {
			t.Fatal("missing auth")
		}
		return a
	}
	location := `{"country":"US","region":"Ohio","city":"Piketon","timezone":"America/New_York"}`
	patch(`"codex_request_location_mode":"on","codex_request_location":`+location, 200)
	data, err := os.ReadFile(filepath.Join(authDir, "account.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]any
	if err = json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if saved["codex_request_location_mode"] != "on" || saved["codex_request_location"] == nil {
		t.Fatal("settings not persisted")
	}
	entry := h.buildAuthFileEntry(get(), authFileQuotaContext{})
	if entry["codex_request_location_mode"] != "on" {
		t.Fatal("account list omits request location mode")
	}
	if got, ok := entry["codex_request_location"].(config.CodexRequestLocation); !ok || got.Timezone != "America/New_York" {
		t.Fatalf("account list location = %#v", entry["codex_request_location"])
	}
	for _, invalid := range []string{
		`"codex_request_location_mode":"local"`,
		`"codex_request_location_mode":true`,
		`"codex_request_location":{"timezone":"America/New_York"}`,
		`"codex_request_location":{"country":"US","region":"Ohio","city":"Piketon","timezone":"Mars/Base"}`,
		`"codex_request_location.timezone":"Asia/Tokyo"`,
		`"codex_request_location_mode.nested":"off"`,
		`"codex_timezone_mode":"egress"`,
		`"codex_timezone":"Asia/Tokyo"`,
	} {
		patch(invalid, 400)
	}
	if get().Metadata["codex_request_location_mode"] != "on" {
		t.Fatal("invalid patch changed live metadata")
	}
	patch(`"codex_request_location_mode":"off"`, 200)
	patch(`"codex_request_location_mode":"egress"`, 200)
	patch(`"codex_request_location_mode":null,"codex_request_location":null`, 200)
	policy, err := config.CodexRequestLocationFromMetadata(get().Metadata)
	if err != nil || policy.Mode != "inherit" || policy.Location != nil {
		t.Fatalf("inherit failed: %+v %v", policy, err)
	}
	if entry := h.buildAuthFileEntry(get(), authFileQuotaContext{}); entry["codex_request_location_mode"] != "inherit" || entry["codex_request_location"] != nil {
		t.Fatal("account list does not show inheritance")
	}
	if get().Metadata["untouched"] != "keep" {
		t.Fatal("unrelated field changed")
	}
}
