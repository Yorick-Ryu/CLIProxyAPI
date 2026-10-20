package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestAccountOpsUsesV8ManagementAuthentication(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("MANAGEMENT_PASSWORD", "ops-test-password")
	cfg := &config.Config{}
	h := management.NewHandler(cfg, "", nil)
	h.SetLocalPassword("ops-test-password")
	server := &Server{cfg: cfg, engine: gin.New(), mgmt: h}
	server.managementRoutesEnabled.Store(true)
	server.registerManagementRoutes()
	for _, tc := range []struct {
		path, key string
		status    int
	}{
		{"/v8/management/account-ops/capabilities", "", 401},
		{"/v8/management/account-ops/capabilities", "ops-test-password", 503},
		{"/v0/management/account-ops/capabilities", "ops-test-password", 404},
	} {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		req.RemoteAddr = "127.0.0.1:1234"
		if tc.key != "" {
			req.Header.Set("Authorization", "Bearer "+tc.key)
		}
		response := httptest.NewRecorder()
		server.engine.ServeHTTP(response, req)
		if response.Code != tc.status {
			t.Errorf("%s status=%d want=%d", tc.path, response.Code, tc.status)
		}
	}
}
