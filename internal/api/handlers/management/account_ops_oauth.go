package management

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

var errAccountOpsConflict = errors.New("account operations configuration changed")

type accountOpsOAuthGuard struct {
	CanCommit func() bool
	JobID     string
	Name      string
	Version   string
	Email     string
	Workspace string
	Plan      string
	Automatic bool
}

func accountOpsConfigVersion(metadata map[string]any) string {
	values := make(map[string]any)
	for _, key := range []string{"email", "account_id", "plan_type", "proxy_url", "proxy-url", "websockets", "disable_cooling", "priority", "note", "disabled"} {
		if value, exists := metadata[key]; exists {
			values[key] = value
		}
	}
	data, _ := json.Marshal(values)
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func (h *Handler) accountOpsAuthMetadata(name string) (map[string]any, error) {
	if isUnsafeAuthFileName(name) || !strings.HasSuffix(name, ".json") {
		return nil, errAccountOpsConflict
	}
	h.mu.Lock()
	authDir := ""
	if h.cfg != nil {
		authDir = h.cfg.AuthDir
	}
	h.mu.Unlock()
	if authDir == "" {
		return nil, errAccountOpsConflict
	}
	raw, errRead := os.ReadFile(filepath.Join(authDir, name))
	if errRead != nil {
		return nil, errAccountOpsConflict
	}
	var metadata map[string]any
	if json.Unmarshal(raw, &metadata) != nil || metadata == nil || metadata["type"] != "codex" {
		return nil, errAccountOpsConflict
	}
	return metadata, nil
}

func (h *Handler) accountOpsTargets(c *gin.Context) {
	targets := []gin.H{}
	if h.authManager == nil {
		c.JSON(503, gin.H{"error": "unavailable"})
		return
	}
	for _, auth := range h.authManager.List() {
		if auth == nil || auth.Provider != "codex" || coreauth.IsPluginVirtualAuth(auth) {
			continue
		}
		name := auth.FileName
		if name == "" {
			name = auth.ID
		}
		metadata, errRead := h.accountOpsAuthMetadata(name)
		if errRead != nil {
			continue
		}
		email, _ := metadata["email"].(string)
		workspace, _ := metadata["account_id"].(string)
		digest := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(email)) + "\x00" + workspace))
		targets = append(targets, gin.H{"id": hex.EncodeToString(digest[:16]), "email": email, "workspace": workspace, "auth_ref": name, "config_version": accountOpsConfigVersion(metadata), "plan_type": metadata["plan_type"], "disabled": metadata["disabled"] == true})
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(200, gin.H{"items": targets, "guarded_oauth": true, "commit_guard_protocol": 2})
}

func (h *Handler) captureAccountOpsOAuthGuard(c *gin.Context) (*accountOpsOAuthGuard, error) {
	// Guarded acquisition is an explicit v8 contract. Legacy OAuth callers keep
	// their existing acquisition flow and never receive an implicit target.
	if !c.GetBool(ConfigV8ContextKey) || c.Query("ops_email") == "" {
		return nil, nil
	}
	guard := &accountOpsOAuthGuard{Name: c.Query("ops_target"), Version: c.Query("ops_version"), Email: strings.TrimSpace(c.Query("ops_email")), Automatic: c.Query("ops_automatic") == "true"}
	guard.JobID = c.Query("ops_job")
	if guard.JobID == "" {
		return nil, errAccountOpsConflict
	}
	if guard.JobID != "" {
		if len(guard.JobID) != 32 {
			return nil, errAccountOpsConflict
		}
		guard.CanCommit = func() bool { return h.accountOpsJobCanCommit(guard.JobID, guard.Email, guard.Name, guard.Automatic) }
	}

	if !strings.Contains(guard.Email, "@") || strings.ContainsAny(guard.Email, "\r\n") {
		return nil, errAccountOpsConflict
	}
	if guard.Name != "" {
		metadata, errRead := h.accountOpsAuthMetadata(guard.Name)
		if errRead != nil || guard.Version == "" || guard.Version != accountOpsConfigVersion(metadata) || !strings.EqualFold(fmt.Sprint(metadata["email"]), guard.Email) {
			return nil, errAccountOpsConflict
		}
		guard.Workspace, _ = metadata["account_id"].(string)
		guard.Plan, _ = metadata["plan_type"].(string)
		if guard.Workspace == "" || guard.Plan == "" {
			return nil, errAccountOpsConflict
		}
		if guard.Automatic && !accountOpsTermValid(metadata, time.Now()) {
			return nil, errAccountOpsConflict
		}
	} else if guard.Automatic {
		return nil, errAccountOpsConflict
	}
	return guard, nil
}

func accountOpsTermValid(metadata map[string]any, now time.Time) bool {
	if metadata["disabled"] == true || strings.EqualFold(fmt.Sprint(metadata["plan_type"]), "free") {
		return false
	}
	note, _ := metadata["note"].(string)
	var expiryLines []string
	for _, line := range strings.Split(note, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "到期时间：") || strings.HasPrefix(line, "到期时间:") {
			expiryLines = append(expiryLines, line)
		}
	}
	if len(expiryLines) != 1 {
		return false
	}
	line := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(expiryLines[0], "到期时间："), "到期时间:"))
	if len(line) < 16 || strings.ContainsAny(line, "+Z") || strings.Contains(line, "UTC") || strings.Contains(line, "GMT") {
		return false
	}
	zone := time.FixedZone("Asia/Shanghai", 8*60*60)
	end, errParse := time.ParseInLocation("2006-01-02 15:04", line[:16], zone)
	if errParse != nil {
		return false
	}
	suffix := strings.TrimSpace(line[16:])
	if suffix != "" && suffix != "（北京时间）" && suffix != "(北京时间)" {
		return false
	}
	return now.Before(end)
}

func (g *accountOpsOAuthGuard) bind(state string, record *coreauth.Auth) error {
	storage, ok := record.Storage.(*codex.CodexTokenStorage)
	if !ok || storage == nil || !strings.EqualFold(storage.Email, g.Email) {
		return errAccountOpsConflict
	}
	if g.Name != "" {
		if storage.AccountID != g.Workspace || storage.PlanType != g.Plan {
			return errAccountOpsConflict
		}
		record.ID = g.Name
		record.FileName = g.Name
		// Force the old canonical path so validation and replacement share the
		// credential file's inter-process lock, even when upstream naming changes.
		record.Attributes = map[string]string{}
	} else if strings.EqualFold(storage.PlanType, "free") || storage.AccountID == "" {
		return errAccountOpsConflict
	}
	storage.BeforeCommit = func(disk map[string]any) error {
		if g.CanCommit != nil && !g.CanCommit() {
			return errAccountOpsConflict
		}
		if !IsOAuthSessionPending(state, "codex") {
			return errAccountOpsConflict
		}
		if g.Name != "" {
			if disk == nil || accountOpsConfigVersion(disk) != g.Version {
				return errAccountOpsConflict
			}
			if g.Automatic && !accountOpsTermValid(disk, time.Now()) {
				return errAccountOpsConflict
			}
		} else if disk != nil {
			return errAccountOpsConflict
		}
		return nil
	}
	return nil
}

func (h *Handler) accountOpsPatchTarget(c *gin.Context, id string) {
	if h.authManager == nil {
		c.JSON(503, gin.H{"error": "unavailable"})
		return
	}
	var request struct {
		Version string                     `json:"config_version"`
		Fields  map[string]json.RawMessage `json:"fields"`
	}
	if c.ShouldBindJSON(&request) != nil || len(request.Version) != 64 || len(request.Fields) == 0 {
		c.JSON(400, gin.H{"error": "invalid_input"})
		return
	}
	var target *coreauth.Auth
	for _, candidate := range h.authManager.List() {
		if candidate == nil || candidate.Provider != "codex" {
			continue
		}
		metadata, errRead := h.accountOpsAuthMetadata(candidate.FileName)
		if errRead != nil {
			continue
		}
		digest := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(fmt.Sprint(metadata["email"]))) + "\x00" + fmt.Sprint(metadata["account_id"])))
		if hex.EncodeToString(digest[:16]) == id {
			if target != nil {
				c.JSON(409, gin.H{"error": "ambiguous_account"})
				return
			}
			target = candidate.Clone()
		}
	}
	if target == nil {
		c.JSON(404, gin.H{"error": "invalid_input"})
		return
	}
	metadata, errRead := h.accountOpsAuthMetadata(target.FileName)
	if errRead != nil || accountOpsConfigVersion(metadata) != request.Version {
		c.JSON(409, gin.H{"error": "configuration_changed"})
		return
	}
	fields := make(map[string]any)
	for key, raw := range request.Fields {
		switch key {
		case "websockets":
			var value bool
			if json.Unmarshal(raw, &value) != nil {
				c.JSON(400, gin.H{"error": "invalid_input"})
				return
			}
			fields[key] = value
		case "priority":
			var value int
			if json.Unmarshal(raw, &value) != nil || value < -100 || value > 100 {
				c.JSON(400, gin.H{"error": "invalid_input"})
				return
			}
			fields[key] = value
		case "note", "proxy_url":
			var value string
			if json.Unmarshal(raw, &value) != nil || len(value) > 4096 {
				c.JSON(400, gin.H{"error": "invalid_input"})
				return
			}
			fields[key] = value
		default:
			c.JSON(400, gin.H{"error": "invalid_input"})
			return
		}
	}
	for key, value := range fields {
		metadata[key] = value
	}
	target.Metadata = metadata
	target.Storage = nil
	touched := make(map[string]struct{})
	for key := range fields {
		touched[key] = struct{}{}
	}
	syncAuthFileMetadataFields(target, touched)
	ctx := coreauth.WithCredentialCommitGuard(c.Request.Context(), func(disk map[string]any) error {
		if disk == nil || accountOpsConfigVersion(disk) != request.Version {
			return errAccountOpsConflict
		}
		return nil
	})
	updated, errUpdate := h.authManager.Update(ctx, target)
	if errUpdate != nil || updated == nil {
		c.JSON(409, gin.H{"error": "configuration_changed"})
		return
	}
	after, errAfter := h.accountOpsAuthMetadata(target.FileName)
	if errAfter != nil {
		c.JSON(503, gin.H{"error": "unavailable"})
		return
	}
	for key, value := range fields {
		left, _ := json.Marshal(after[key])
		right, _ := json.Marshal(value)
		if string(left) != string(right) {
			c.JSON(409, gin.H{"error": "configuration_changed"})
			return
		}
	}
	if errHook := h.invokePostAuthPersistHooks(ctx, []*coreauth.Auth{updated}); errHook != nil {
		c.JSON(503, gin.H{"error": "unavailable"})
		return
	}
	c.JSON(200, gin.H{"id": id, "config_version": accountOpsConfigVersion(after), "verified": true})
}

func (h *Handler) accountOpsJobCanCommit(jobID, email, target string, automatic bool) bool {
	if len(jobID) != 32 {
		return false
	}
	socketPath := os.Getenv("CPA_ACCOUNT_OPS_SOCKET")
	tokenPath := os.Getenv("CPA_ACCOUNT_OPS_TOKEN_FILE")
	info, errStat := os.Lstat(tokenPath)
	if socketPath == "" || errStat != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return false
	}
	token, errToken := os.ReadFile(tokenPath)
	if errToken != nil {
		return false
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
	}, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	request, errRequest := http.NewRequest(http.MethodGet, "http://account-ops/_internal/jobs/"+jobID+"/guard", nil)
	if errRequest != nil {
		return false
	}
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	response, errDo := (&http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(request)
	if errDo != nil {
		return false
	}
	defer func() { _ = response.Body.Close() }()
	var guard struct {
		Allowed   bool   `json:"allowed"`
		Email     string `json:"email"`
		Target    string `json:"target"`
		Automatic bool   `json:"automatic"`
	}
	if response.StatusCode != 200 || json.NewDecoder(io.LimitReader(response.Body, 16384)).Decode(&guard) != nil {
		return false
	}
	return guard.Allowed && strings.EqualFold(guard.Email, email) && guard.Target == target && guard.Automatic == automatic
}
