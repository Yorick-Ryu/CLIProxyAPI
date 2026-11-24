package executor

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// CodexExecutor is a stateless executor for Codex (OpenAI Responses API entrypoint).
// If api_key is unavailable on auth, it falls back to legacy via ClientAdapter.
type CodexExecutor struct {
	cfg *config.Config
}

func NewCodexExecutor(cfg *config.Config) *CodexExecutor { return &CodexExecutor{cfg: cfg} }

func (e *CodexExecutor) Identifier() string { return "codex" }

type codexErrorPolicy struct {
	modelLevelCooling bool
	legacyRetry       bool
	cooldownLimit     time.Duration
}

func (e *CodexExecutor) errorPolicy() codexErrorPolicy {
	if e == nil || e.cfg == nil {
		return codexErrorPolicy{}
	}
	return codexErrorPolicy{
		modelLevelCooling: e.cfg.Codex.ModelLevelCooling,
		legacyRetry:       !e.cfg.RespectTransientErrorRetryAfter(),
		cooldownLimit:     e.cfg.TransientErrorCooldownLimit(),
	}
}

func (p codexErrorPolicy) isTransientCapacityError(body []byte, headers http.Header) bool {
	if p.legacyRetry {
		seconds, err := strconv.ParseUint(strings.TrimSpace(headers.Get("Retry-After")), 10, 64)
		if err != nil || seconds != 0 {
			return false
		}
	}
	return isCodexTransientCapacityError(body)
}

func (p codexErrorPolicy) isCapacityError(body []byte, headers http.Header) bool {
	return p.isTransientCapacityError(body, headers) || (p.legacyRetry && isCodexModelCapacityError(body))
}

// SupportsApplyPatch reports the actual executor contract, independent of its provider name.
func (e *CodexExecutor) SupportsApplyPatch() bool { return e != nil }
