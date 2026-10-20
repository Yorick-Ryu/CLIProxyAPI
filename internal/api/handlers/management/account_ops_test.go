package management

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestAccountOpsLocalTransport(t *testing.T) {
	gin.SetMode(gin.TestMode)
	// macOS Unix socket paths have a small limit; Go's descriptive TempDir can exceed it.
	dir, err := os.MkdirTemp("", "ops-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	socket := filepath.Join(dir, "worker.sock")
	token := filepath.Join(dir, "service-token")
	secret := strings.Repeat("s", 40)
	if errWrite := os.WriteFile(token, []byte(secret), 0600); errWrite != nil {
		t.Fatal(errWrite)
	}
	listener, errListen := net.Listen("unix", socket)
	if errListen != nil {
		t.Skipf("unix socket not available: %v", errListen)
	}
	t.Cleanup(func() { _ = listener.Close() })
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+secret {
			t.Error("service authentication not injected")
		}
		if r.Header.Get("Cookie") != "" {
			t.Error("browser cookie reached worker")
		}
		data, _ := io.ReadAll(r.Body)
		if r.Method == http.MethodPost && string(data) != `{"kind":"record"}` {
			t.Errorf("body=%q", data)
		}
		if r.URL.Path == "/records" {
			w.WriteHeader(500)
			_, _ = w.Write([]byte(`{"error":"private-token-value"}`))
			return
		}
		w.Header().Set("Set-Cookie", "private=never")
		_, _ = w.Write([]byte(`{"worker_available":true,"browser":false}`))
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	t.Setenv("CPA_ACCOUNT_OPS_SOCKET", socket)
	t.Setenv("CPA_ACCOUNT_OPS_TOKEN_FILE", token)
	h := &Handler{}
	engine := gin.New()
	engine.Any("/account-ops/*path", h.AccountOps)
	call := func(method, path, body, origin string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "http://localhost/account-ops"+path, strings.NewReader(body))
		req.Header.Set("Origin", origin)
		req.Header.Set("Authorization", "Bearer browser-key")
		req.Header.Set("Cookie", "session=private")
		response := httptest.NewRecorder()
		engine.ServeHTTP(response, req)
		return response
	}
	if response := call("GET", "/capabilities", "", ""); response.Code != 200 || response.Header().Get("Set-Cookie") != "" {
		t.Fatalf("capabilities status=%d", response.Code)
	}
	if response := call("POST", "/jobs", `{"kind":"record"}`, "http://localhost"); response.Code != 200 {
		t.Fatalf("post status=%d", response.Code)
	}
	if response := call("GET", "/capabilities", "", "https://attacker.example"); response.Code != 403 {
		t.Fatalf("cross origin status=%d", response.Code)
	}
	if response := call("GET", "/records", "", ""); response.Code != 503 || strings.Contains(response.Body.String(), "private-token-value") {
		t.Fatal("worker failure leaked or had wrong status")
	}
	if response := call("GET", "/credentials", "", ""); response.Code != 404 {
		t.Fatalf("arbitrary endpoint status=%d", response.Code)
	}
	if response := call("POST", "/jobs", strings.Repeat("x", 300001), ""); response.Code != 413 {
		t.Fatalf("oversized body status=%d", response.Code)
	}
	if errChmod := os.Chmod(token, 0644); errChmod != nil {
		t.Fatal(errChmod)
	}
	if response := call("GET", "/capabilities", "", ""); response.Code != 503 {
		t.Fatalf("public token file status=%d", response.Code)
	}
}
