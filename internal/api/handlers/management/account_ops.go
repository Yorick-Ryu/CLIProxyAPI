package management

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"
)

var accountOpsRoute = regexp.MustCompile(`^/(capabilities|monitor|records|accounts|schedules(/(quality_inspection|health_scan|auto_reauth_scan))?|imports/preview|jobs|jobs/[a-f0-9]{32}(/(events|cancel|resume))?|preferences/[a-zA-Z0-9_.-]{1,64}|accounts/[a-f0-9]{32}/credentials)$`)

// AccountOps bridges only the account-operations protocol to a protected local
// socket. No client-supplied hostname, path or credential reaches the transport.
func (h *Handler) AccountOps(c *gin.Context) {
	path := c.Param("path")
	if parts := strings.Split(strings.Trim(path, "/"), "/"); len(parts) == 3 && parts[0] == "targets" && len(parts[1]) == 32 && parts[2] == "preferences" && c.Request.Method == http.MethodPost {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16000)
		h.accountOpsPatchTarget(c, parts[1])
		return
	}
	if path == "/targets" && c.Request.Method == http.MethodGet {
		h.accountOpsTargets(c)
		return
	}
	if !accountOpsRoute.MatchString(path) {
		c.JSON(http.StatusNotFound, gin.H{"error": "invalid_input"})
		return
	}
	if origin := c.GetHeader("Origin"); origin != "" {
		parsed, errParse := url.Parse(origin)
		if errParse != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || !strings.EqualFold(parsed.Host, c.Request.Host) || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" {
			c.JSON(http.StatusForbidden, gin.H{"error": "invalid_input"})
			return
		}
	}
	socketPath := os.Getenv("CPA_ACCOUNT_OPS_SOCKET")
	tokenPath := os.Getenv("CPA_ACCOUNT_OPS_TOKEN_FILE")
	info, errStat := os.Lstat(tokenPath)
	if socketPath == "" || !filepath.IsAbs(socketPath) || errStat != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "unavailable", "worker_available": false})
		return
	}
	secret, errRead := os.ReadFile(tokenPath)
	if errRead != nil || len(strings.TrimSpace(string(secret))) < 32 {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "unavailable", "worker_available": false})
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 300000)
	body, errInput := io.ReadAll(c.Request.Body)
	if errInput != nil {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "invalid_input"})
		return
	}
	req, errRequest := http.NewRequestWithContext(c.Request.Context(), c.Request.Method, "http://account-ops"+path, bytes.NewReader(body))
	if errRequest != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_input"})
		return
	}
	req.URL.RawQuery = c.Request.URL.RawQuery
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(secret)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", c.GetHeader("Idempotency-Key"))
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
	}, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, errDo := client.Do(req)
	if errDo != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "unavailable", "worker_available": false})
		return
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		status := response.StatusCode
		if status < 400 || status >= 500 {
			status = http.StatusServiceUnavailable
		}
		var failure struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&failure)
		reason := "failed"
		for _, allowed := range []string{"invalid_input", "unavailable", "conflict", "locked", "draft_expired", "configuration_changed"} {
			if failure.Error == allowed {
				reason = allowed
			}
		}
		c.JSON(status, gin.H{"error": reason})
		return
	}
	data, errBody := io.ReadAll(io.LimitReader(response.Body, 4*1024*1024+1))
	if errBody != nil || len(data) > 4*1024*1024 || !json.Valid(data) {
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed"})
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Data(response.StatusCode, "application/json", data)
}
