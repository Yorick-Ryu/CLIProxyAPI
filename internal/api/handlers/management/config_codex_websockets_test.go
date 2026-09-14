package management

import (
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPatchCodexKeyWebsocketsOverride(t *testing.T) {
	h := &Handler{cfg: &config.Config{CodexKey: []config.CodexKey{{APIKey: "test", BaseURL: "https://example.invalid/v1"}}}, configFilePath: writeTestConfigFile(t)}
	for _, tc := range []struct {
		value  string
		want   *bool
		status int
	}{
		{`{"websockets":false}`, func() *bool { v := false; return &v }(), 200},
		{`{}`, func() *bool { v := false; return &v }(), 200},
		{`{"websockets":true}`, func() *bool { v := true; return &v }(), 200},
		{`{"websockets":null}`, nil, 200},
		{`{"websockets":"invalid"}`, nil, 400},
	} {
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/codex-api-key", strings.NewReader(`{"index":0,"value":`+tc.value+`}`))
		ctx.Request.Header.Set("Content-Type", "application/json")
		h.PatchCodexKey(ctx)
		if rec.Code != tc.status {
			t.Fatalf("patch %s status=%d: %s", tc.value, rec.Code, rec.Body.String())
		}
		got := h.cfg.CodexKey[0].Websockets
		if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
			t.Fatalf("patch %s lost tri-state override", tc.value)
		}
	}
}
