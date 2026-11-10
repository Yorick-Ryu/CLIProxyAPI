package executor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"runtime"
	"strconv"
	"strings"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

const (
	codexUserAgent             = "codex-tui/0.162.0 (Ubuntu 24.4.0; x86_64) xterm-256color (codex-tui; 0.162.0)"
	codexOriginator            = "codex-tui"
	codexDefaultImageToolModel = "gpt-image-2"
	codexResponsesLiteHeader   = "X-OpenAI-Internal-Codex-Responses-Lite"
)

var dataTag = []byte("data:")

func translateCodexRequestPair(from, to sdktranslator.Format, model string, originalPayload, payload []byte, stream bool, preserveEmptyThinkingBlocks ...bool) ([]byte, []byte) {
	original, body, _, _ := translateCodexRequestPairWithUpdateIntent(from, to, model, originalPayload, payload, stream, preserveEmptyThinkingBlocks...)
	return original, body
}

func translateCodexRequestPairWithUpdateIntent(from, to sdktranslator.Format, model string, originalPayload, payload []byte, stream bool, preserveEmptyThinkingBlocks ...bool) ([]byte, []byte, bool, error) {
	return translateCodexRequestPairWithUpdateIntentContext(context.Background(), from, to, model, originalPayload, payload, stream, preserveEmptyThinkingBlocks...)
}

func translateCodexRequestPairWithUpdateIntentContext(ctx context.Context, from, to sdktranslator.Format, model string, originalPayload, payload []byte, stream bool, preserveEmptyThinkingBlocks ...bool) ([]byte, []byte, bool, error) {
	isCompat := len(preserveEmptyThinkingBlocks) > 0 && preserveEmptyThinkingBlocks[0]
	if ctx == nil {
		ctx = context.Background()
	}
	translate := func(raw []byte) ([]byte, bool, error) {
		if isCompat && from == sdktranslator.FormatClaude && to == sdktranslator.FormatCodex {
			body, err := helps.TranslateRequestReturningError(ctx, nil, nil, from, to, model, raw, stream, true)
			return body, false, err
		}
		translated := sdktranslator.TranslateRequestEnvelope(ctx, from, to, sdktranslator.RequestEnvelope{Format: from, Model: model, Stream: stream, Body: raw})
		return translated.Body, translated.ConfigurationUpdatesChanged, translated.Err
	}
	if bytes.Equal(originalPayload, payload) {
		body, changed, err := translate(payload)
		return body, body, changed, err
	}
	originalTranslated, _, _ := translate(originalPayload)
	body, changed, err := translate(payload)
	return originalTranslated, body, changed, err
}

// codexMinOutputTokens is the smallest integer max_output_tokens value the ChatGPT
// Codex backend accepts; smaller integers are rejected with invalid_request_error.
const codexMinOutputTokens = 16

// normalizeCodexOutputBudgetDoc raises small positive integer output budgets to the
// Codex minimum. It judges the translated payload (max_output_tokens) rather than the
// client payload so every source format and budget field (max_tokens,
// max_completion_tokens, generation_config, ...) is covered uniformly. Budgets that
// are missing, non-numeric, non-integral, or <= 0 are left untouched for the upstream
// to reject. This runs before user payload rules so overrides and filters stay final.
func normalizeCodexOutputBudgetDoc(doc *ojson.Doc) {
	if doc == nil {
		return
	}
	translated := doc.Get("max_output_tokens")
	if translated.Type != ojson.Number {
		return
	}
	tokens, errFloat := strconv.ParseFloat(translated.Raw, 64)
	if errFloat != nil || tokens < 1 || tokens >= codexMinOutputTokens || tokens != math.Trunc(tokens) {
		return
	}
	_ = doc.Set("max_output_tokens", codexMinOutputTokens)
}

// normalizeCodexOutputBudget preserves the byte pipeline when no adjustment is needed.
func normalizeCodexOutputBudget(body []byte) []byte {
	doc := ojson.NewDoc(body)
	normalizeCodexOutputBudgetDoc(doc)
	if ojson.GetBytesNoCopy(body, "max_output_tokens").Raw == doc.Get("max_output_tokens").Raw {
		return body
	}
	return doc.Bytes()
}

func codexHostedImageRequestedModel(requested, fallback string) string {
	if strings.TrimSpace(requested) != "" {
		return requested
	}
	return fallback
}

// PrepareRequest injects Codex credentials into the outgoing HTTP request.
func (e *CodexExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	if auth != nil && !codexAuthUsesAPIKey(auth) {
		helps.StripCodexTraceContextHeaders(req.Header)
	}
	apiKey, _ := codexCredsForRequest(req.Context(), auth, req.Header)
	if codexUseSIWCForRequest(req.Context(), auth, req.Header) && req.URL != nil && req.URL.Scheme == "https" &&
		req.URL.Host == "chatgpt.com" && req.URL.Path == "/backend-api/codex/alpha/search" {
		// ChatGPT Alpha Search requires the original OAuth partition, not SIWC.
		apiKey, _ = auth.Metadata["access_token"].(string)
		if strings.TrimSpace(apiKey) == "" {
			return statusErr{code: http.StatusUnauthorized, msg: "codex executor: original OAuth access_token is required for ChatGPT Alpha Search"}
		}
	} else if errSIWC := ensureCodexSIWCCredentialsForRequest(req.Context(), auth, apiKey, req.Header); errSIWC != nil {
		return errSIWC
	}
	if strings.TrimSpace(apiKey) != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	} else {
		req.Header.Del("Authorization")
	}
	if e.codexAlphaSearchCompatibility(req, auth) {
		if err := e.prepareCodexAlphaSearch(req, auth, apiKey); err != nil {
			return err
		}
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	if e.codexAlphaSearchCompatibility(req, auth) {
		prepared, _ := req.Context().Value(codexAlphaSearchPreparedKey{}).(*codexAlphaSearchPrepared)
		source := prepared.source
		util.ApplyCustomHeadersFromAttrs(req, attrs, source)
		if req.Header.Get("Version") == "" || (!isCodexCloakingDisabled(e.cfg, auth) && strings.TrimSpace(e.cfg.CodexHeaderDefaults.Version) == "" && codexOperatorHeaderValue(req.Context(), auth, source, "Version") == "") {
			for _, part := range strings.Fields(req.Header.Get("User-Agent")) {
				if strings.HasPrefix(part, "codex") {
					if _, version, ok := strings.Cut(part, "/"); ok {
						req.Header.Set("Version", version)
						break
					}
				}
			}
		}
		applyCodexAccountHeaders(req.Context(), req.Header, e.cfg, auth, source)
	} else {
		util.ApplyCustomHeadersFromAttrs(req, attrs)
	}
	return nil
}

// HttpRequest injects Codex credentials into the request and executes it.
func (e *CodexExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("codex executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	// NewHttpRequest may already have scoped the search request. Carry the
	// private preparation marker when the caller supplies an enriched context.
	if prepared := req.Context().Value(codexAlphaSearchPreparedKey{}); prepared != nil {
		ctx = context.WithValue(ctx, codexAlphaSearchPreparedKey{}, prepared)
	}
	httpReq := req.Clone(ctx)
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}
	httpClient := helps.NewCodexHTTPClient(ctx, e.cfg, auth, 0)
	return httpClient.Do(httpReq)
}

type codexAlphaSearchPreparedKey struct{}

type codexAlphaSearchPrepared struct {
	scope  string
	source http.Header
	body   []byte
}

func (e *CodexExecutor) codexAlphaSearchCompatibility(req *http.Request, auth *cliproxyauth.Auth) bool {
	if req == nil {
		return false
	}
	source := req.Header
	if prepared, _ := req.Context().Value(codexAlphaSearchPreparedKey{}).(*codexAlphaSearchPrepared); prepared != nil {
		source = prepared.source
	}
	return req.URL != nil && req.Method == http.MethodPost && strings.HasSuffix(req.URL.EscapedPath(), "/alpha/search") &&
		e.cfg != nil && strings.EqualFold(strings.TrimSpace(e.cfg.Codex.HTTPTLSProfile), "linux") &&
		auth != nil && strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") && !codexAuthUsesAPIKey(auth) && !codexUseSIWCForRequest(req.Context(), auth, source)
}

// prepareCodexAlphaSearch keeps native search metadata and its request id in
// the same account namespace as Responses, without adding Responses
// session headers or payload fields. Preparation can run twice in the manager.
func (e *CodexExecutor) prepareCodexAlphaSearch(req *http.Request, auth *cliproxyauth.Auth, token string) error {
	identity := resolveCodexAccountIdentityState(auth)
	prepared, _ := req.Context().Value(codexAlphaSearchPreparedKey{}).(*codexAlphaSearchPrepared)
	if prepared != nil && prepared.scope == identity.namespace {
		return nil
	}
	source := req.Header.Clone()
	var body []byte
	if prepared != nil {
		source, body = prepared.source.Clone(), prepared.body
	} else {
		if g, ok := req.Context().Value("gin").(*gin.Context); ok && g != nil && g.Request != nil {
			source = g.Request.Header.Clone()
		}
		if req.Body != nil {
			reader := req.Body
			if req.GetBody != nil {
				var err error
				reader, err = req.GetBody()
				if err != nil {
					return err
				}
			}
			var err error
			body, err = io.ReadAll(reader)
			_ = reader.Close()
			if err != nil {
				return err
			}
		}
	}
	// Rebuild only native search headers. Explicit account attributes are applied
	// by PrepareRequest afterwards and retain precedence over the defaults.
	headers := make(http.Header)
	for _, name := range []string{"Version", "X-Codex-Turn-Metadata"} {
		if value := source.Get(name); value != "" {
			headers.Set(name, value)
		}
	}
	headers.Set("Content-Type", "application/json")
	accept := source.Get("Accept")
	if accept == "" {
		accept = "*/*"
	}
	headers.Set("Accept", accept)
	ua, _ := codexHeaderDefaults(e.cfg, auth)
	ensureHeaderWithConfigPrecedence(headers, source, "User-Agent", ua, codexUserAgent)
	origin := source.Get("Originator")
	if origin == "" {
		origin = codexOriginator
	}
	headers.Set("Originator", origin)
	if version := strings.TrimSpace(e.cfg.CodexHeaderDefaults.Version); version != "" {
		headers.Set("Version", version)
	}
	if accountID, _ := auth.Metadata["account_id"].(string); accountID != "" {
		headers.Set("Chatgpt-Account-Id", accountID)
	}
	if token != "" {
		headers.Set("Authorization", "Bearer "+token)
	}
	mapped, identity := applyCodexAccountIdentityBodyForConfig(req.Context(), e.cfg, auth, body)
	if id := ojson.GetBytes(mapped, "id"); id.Type == ojson.String && id.String() != "" {
		mapped = helps.SetStringIfDifferent(mapped, "id", codexAccountIdentityUUID(identity, "session", id.String()))
	}
	applyCodexAccountIdentityHeaders(headers, &identity)
	applyCodexCloakingHeaders(headers, e.cfg, auth)
	if metadata := headers.Get("X-Codex-Turn-Metadata"); metadata != "" {
		headers.Set("X-Codex-Turn-Metadata", helps.CodexSearchTurnMetadata(metadata))
	}
	mapped = helps.StripCodexTraceContextBody(mapped)
	helps.StripCodexTraceContextHeaders(headers)
	mapped = helps.FinalizePayload(req.Context(), mapped)
	req.Header = headers
	req.Body = io.NopCloser(bytes.NewReader(mapped))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(mapped)), nil }
	req.ContentLength = int64(len(mapped))
	mark := &codexAlphaSearchPrepared{scope: resolveCodexAccountIdentityState(auth).namespace, source: source, body: body}
	*req = *req.WithContext(context.WithValue(req.Context(), codexAlphaSearchPreparedKey{}, mark))
	return nil
}

type codexIdentityConfuseState struct {
	enabled                bool
	canonicalMetadata      bool
	authID                 string
	originalPromptCacheKey string
	promptCacheKey         string
	turnIDs                []codexIdentityReplacement
	accountIdentity        codexAccountIdentityState
	convergence            codexIdentityConvergenceState
}

type codexIdentityReplacement struct {
	original string
	confused string
}

func (e *CodexExecutor) cacheHelper(ctx context.Context, from sdktranslator.Format, url string, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, userPayload []byte, rawJSON []byte, headerSets ...http.Header) (*http.Request, []byte, codexIdentityConfuseState, error) {
	httpReq, body, identityState, err := e.prepareCachedRequest(ctx, from, url, auth, req, userPayload, rawJSON, headerSets...)
	if err != nil {
		return nil, nil, codexIdentityConfuseState{}, err
	}
	if err = e.setCodexRequestBody(httpReq, auth, body, headerSets...); err != nil {
		return nil, nil, codexIdentityConfuseState{}, err
	}
	return httpReq, body, identityState, nil
}

func (e *CodexExecutor) prepareCachedRequest(ctx context.Context, from sdktranslator.Format, url string, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, userPayload []byte, rawJSON []byte, headerSets ...http.Header) (*http.Request, []byte, codexIdentityConfuseState, error) {
	rawJSON = helps.ApplyCodexRequestLocation(ctx, e.cfg, auth, rawJSON)
	var headers http.Header
	if len(headerSets) > 0 {
		headers = headerSets[0]
	}
	var cache helps.CodexCache
	if sourceFormatEqual(from, sdktranslator.FormatClaude) {
		modelName := strings.TrimSpace(ojson.GetBytes(rawJSON, "model").String())
		if modelName == "" {
			modelName = thinking.ParseSuffix(req.Model).ModelName
		}
		cached, ok, errCache := helps.ClaudeCodePromptCache(ctx, modelName, req.Payload, headers)
		if errCache != nil {
			return nil, nil, codexIdentityConfuseState{}, errCache
		}
		if ok {
			cache = cached
		}
	} else if sourceFormatEqual(from, sdktranslator.FormatOpenAIResponse) {
		promptCacheKey := ojson.GetBytes(req.Payload, "prompt_cache_key")
		if promptCacheKey.Exists() {
			cache.ID = promptCacheKey.String()
		}
	} else if sourceFormatEqual(from, sdktranslator.FormatOpenAI) {
		if promptCacheKey := ojson.GetBytes(req.Payload, "prompt_cache_key"); promptCacheKey.Exists() {
			cache.ID = strings.TrimSpace(promptCacheKey.String())
		}
		if cache.ID == "" {
			cache.ID = helps.ProviderSessionUUID("codex", req.Metadata)
		}
		if cache.ID == "" {
			if apiKey := strings.TrimSpace(helps.APIKeyFromContext(ctx)); apiKey != "" {
				cache.ID = uuid.NewSHA1(uuid.NameSpaceOID, []byte("cli-proxy-api:codex:prompt-cache:"+apiKey)).String()
			}
		}
	}
	if cache.ID == "" {
		cache.ID = helps.ProviderSessionUUID("codex", req.Metadata)
	}

	if cache.ID != "" {
		rawJSON = helps.SetStringIfDifferent(rawJSON, "prompt_cache_key", cache.ID)
	}
	rawJSON = helps.SanitizeCodexInputItemIDs(rawJSON)
	rawJSON, accountIdentityState := applyCodexAccountIdentityBodyForConfig(ctx, e.cfg, auth, rawJSON)
	var identityState codexIdentityConfuseState
	rawJSON, identityState = applyCodexIdentityConfuseBody(e.cfg, auth, userPayload, rawJSON)
	identityState.accountIdentity = accountIdentityState
	convergenceSourcePayload := userPayload
	if accountIdentityState.enabled {
		convergenceSourcePayload = rawJSON
	}
	rawJSON, identityState.convergence = applyCodexIdentityConvergenceBody(e.cfg, auth, convergenceSourcePayload, rawJSON, headers)
	if identityState.accountIdentity.promptCacheKeyWasRemapped {
		cache.ID = identityState.accountIdentity.promptCacheKey
	}
	if identityState.promptCacheKey != "" {
		cache.ID = identityState.promptCacheKey
	}
	if identityState.convergence.promptCacheKeyWasConverged {
		cache.ID = identityState.convergence.sessionID
	}
	if auth != nil && !codexAuthUsesAPIKey(auth) {
		rawJSON = helps.PrepareCodexRequestMetadata(rawJSON)
	}
	rawJSON = helps.GuardCodexTurnStateBody(ctx, rawJSON)
	rawJSON = helps.FinalizePayload(ctx, rawJSON)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return nil, nil, codexIdentityConfuseState{}, err
	}
	if cache.ID != "" {
		sessionID := cache.ID
		if codexUseSIWCForRequest(ctx, auth, headers) && accountIdentityState.promptCacheKeyWasRemapped {
			// SIWC copies this header into X-Client-Request-Id before the
			// account header mapping, so both must start from the original ID.
			sessionID = accountIdentityState.originalPromptCacheKey
		}
		httpReq.Header.Set("Session-Id", sessionID)
	}
	return httpReq, rawJSON, identityState, nil
}

func (e *CodexExecutor) setCodexRequestBody(httpReq *http.Request, auth *cliproxyauth.Auth, body []byte, clientHeaders ...http.Header) error {
	wireBody := body
	compressed := codexRequestCompressionEnabled(e.cfg, auth, httpReq, len(body), clientHeaders...)
	if compressed {
		var errCompress error
		wireBody, errCompress = helps.CompressCodexRequestBody(body)
		if errCompress != nil {
			return errCompress
		}
	}
	httpReq.Body = io.NopCloser(bytes.NewReader(wireBody))
	httpReq.ContentLength = int64(len(wireBody))
	httpReq.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(wireBody)), nil
	}
	httpReq.TransferEncoding = nil
	httpReq.Header.Del("Content-Length")
	httpReq.Header.Del("Content-Encoding")
	if compressed {
		httpReq.Header.Set("Content-Encoding", "zstd")
	}
	return nil
}

func codexRequestCompressionEnabled(cfg *config.Config, auth *cliproxyauth.Auth, req *http.Request, bodyBytes int, clientHeaders ...http.Header) bool {
	if cfg == nil || !cfg.Codex.RequestCompression.Enabled || auth == nil {
		return false
	}
	if codexAuthUsesAPIKey(auth) || req == nil || req.URL == nil || req.Method != http.MethodPost || codexUseSIWCForRequest(req.Context(), auth, clientHeaders...) {
		return false
	}
	// Native Codex enables zstd on Responses HTTP, not legacy compaction.
	// Keep the proxy's direct-image extension, after its final JSON conversion.
	path := req.URL.Path
	if !strings.HasSuffix(path, "/responses") && !strings.HasSuffix(path, "/images/generations") && !strings.HasSuffix(path, "/images/edits") {
		return false
	}
	minBytes := cfg.Codex.RequestCompression.MinBytes
	if minBytes < 0 {
		minBytes = 0
	}
	return bodyBytes >= minBytes
}

func applyCodexIdentityConfuseBody(cfg *config.Config, auth *cliproxyauth.Auth, userPayload []byte, rawJSON []byte) ([]byte, codexIdentityConfuseState) {
	state := codexIdentityConfuseState{canonicalMetadata: auth != nil && !codexAuthUsesAPIKey(auth)}
	if codexIdentityConvergenceEnabled(cfg, auth) || !codexIdentityConfuseEnabled(cfg) || auth == nil || strings.TrimSpace(auth.ID) == "" || len(rawJSON) == 0 {
		return rawJSON, state
	}

	state.enabled = true
	state.authID = strings.TrimSpace(auth.ID)
	if promptCacheKey := strings.TrimSpace(ojson.GetBytes(userPayload, "prompt_cache_key").String()); promptCacheKey != "" {
		state.originalPromptCacheKey = promptCacheKey
		state.promptCacheKey = codexIdentityConfuseUUID(auth.ID, "prompt-cache", promptCacheKey)
		rawJSON = helps.SetStringIfDifferent(rawJSON, "prompt_cache_key", state.promptCacheKey)
	}
	if installationID := strings.TrimSpace(ojson.GetBytes(userPayload, "client_metadata.x-codex-installation-id").String()); installationID != "" {
		rawJSON, _ = ojson.SetBytes(rawJSON, "client_metadata.x-codex-installation-id", codexIdentityConfuseUUID(auth.ID, "installation", installationID))
	}
	if turnMetadata := strings.TrimSpace(ojson.GetBytes(rawJSON, "client_metadata.x-codex-turn-metadata").String()); turnMetadata != "" {
		rawJSON, _ = ojson.SetBytes(rawJSON, "client_metadata.x-codex-turn-metadata", applyCodexTurnMetadataIdentityConfuse(turnMetadata, &state))
	}
	if state.promptCacheKey != "" {
		if windowID := strings.TrimSpace(ojson.GetBytes(rawJSON, "client_metadata.x-codex-window-id").String()); windowID != "" {
			rawJSON, _ = ojson.SetBytes(rawJSON, "client_metadata.x-codex-window-id", state.promptCacheKey+":0")
		}
	}

	return rawJSON, state
}

func applyCodexIdentityConfuseHeaders(headers http.Header, state *codexIdentityConfuseState) {
	if headers == nil || state == nil {
		return
	}
	if state.canonicalMetadata {
		defer helps.CanonicalCodexTurnMetadataHeaders(headers)
	}
	applyCodexAccountIdentityHeaders(headers, &state.accountIdentity)
	applyCodexIdentityConvergenceHeaders(headers, &state.convergence)
	if !state.enabled {
		return
	}

	if rawTurnMetadata := strings.TrimSpace(headers.Get("X-Codex-Turn-Metadata")); rawTurnMetadata != "" {
		headers.Set("X-Codex-Turn-Metadata", applyCodexTurnMetadataIdentityConfuse(rawTurnMetadata, state))
	}
	if state.promptCacheKey == "" {
		return
	}

	setCodexSessionHeaderCasePreserved(headers, "Session-Id", state.promptCacheKey)
	if headerValueCaseInsensitive(headers, "Conversation_id") != "" {
		setHeaderCasePreserved(headers, "Conversation_id", state.promptCacheKey)
	}
	headers.Set("X-Client-Request-Id", state.promptCacheKey)
	headers.Set("Thread-Id", state.promptCacheKey)
	headers.Set("X-Codex-Window-Id", state.promptCacheKey+":0")
}

func applyCodexTurnMetadataIdentityConfuse(rawTurnMetadata string, state *codexIdentityConfuseState) string {
	updatedTurnMetadata := rawTurnMetadata
	if state == nil || !state.enabled {
		return updatedTurnMetadata
	}
	if state.promptCacheKey != "" && ojson.Get(rawTurnMetadata, "prompt_cache_key").Exists() {
		updatedTurnMetadata, _ = ojson.Set(updatedTurnMetadata, "prompt_cache_key", state.promptCacheKey)
	} else if state.promptCacheKey != "" && state.originalPromptCacheKey != "" {
		updatedTurnMetadata = strings.ReplaceAll(updatedTurnMetadata, state.originalPromptCacheKey, state.promptCacheKey)
	}
	if turnID := strings.TrimSpace(ojson.Get(rawTurnMetadata, "turn_id").String()); turnID != "" {
		updatedTurnMetadata, _ = ojson.Set(updatedTurnMetadata, "turn_id", state.confuseTurnID(turnID))
	}
	if state.promptCacheKey != "" && ojson.Get(rawTurnMetadata, "window_id").Exists() {
		updatedTurnMetadata, _ = ojson.Set(updatedTurnMetadata, "window_id", state.promptCacheKey+":0")
	}
	return updatedTurnMetadata
}

func applyCodexIdentityConfuseResponsePayload(payload []byte, state codexIdentityConfuseState) []byte {
	payload = replaceCodexIdentityResponsePayload(payload, state.originalPromptCacheKey, state.promptCacheKey)
	for _, turnID := range state.turnIDs {
		payload = replaceCodexIdentityResponsePayload(payload, turnID.original, turnID.confused)
	}
	return payload
}

func applyCodexIdentityExposeResponsePayload(payload []byte, state codexIdentityConfuseState) []byte {
	payload = replaceCodexIdentityResponsePayload(payload, state.promptCacheKey, state.originalPromptCacheKey)
	for _, turnID := range state.turnIDs {
		payload = replaceCodexIdentityResponsePayload(payload, turnID.confused, turnID.original)
	}
	return payload
}

func (state *codexIdentityConfuseState) confuseTurnID(turnID string) string {
	turnID = strings.TrimSpace(turnID)
	if state == nil || !state.enabled || strings.TrimSpace(state.authID) == "" || turnID == "" {
		return turnID
	}
	for _, replacement := range state.turnIDs {
		if replacement.original == turnID || replacement.confused == turnID {
			return replacement.confused
		}
	}
	confusedTurnID := codexIdentityConfuseUUID(state.authID, "turn", turnID)
	state.turnIDs = append(state.turnIDs, codexIdentityReplacement{original: turnID, confused: confusedTurnID})
	return confusedTurnID
}

func replaceCodexIdentityResponsePayload(payload []byte, from string, to string) []byte {
	from = strings.TrimSpace(from)
	to = strings.TrimSpace(to)
	if len(payload) == 0 || from == "" || to == "" || from == to || !bytes.Contains(payload, []byte(from)) {
		return payload
	}
	return bytes.ReplaceAll(payload, []byte(from), []byte(to))
}

func codexIdentityConfuseEnabled(cfg *config.Config) bool {
	if cfg == nil || !cfg.Codex.IdentityConfuse {
		return false
	}
	strategy := strings.ToLower(strings.TrimSpace(cfg.Routing.Strategy))
	return cfg.Routing.SessionAffinity || strategy == "fill-first" || strategy == "fillfirst" || strategy == "ff"
}

func codexIdentityConfuseUUID(authID string, kind string, value string) string {
	name := strings.Join([]string{"cli-proxy-api", "codex", "identity-confuse", kind, strings.TrimSpace(authID), strings.TrimSpace(value)}, ":")
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(name)).String()
}

func applyCodexHeaders(r *http.Request, auth *cliproxyauth.Auth, token string, stream bool, cfg *config.Config, clientHeaders ...http.Header) {
	var ginHeaders http.Header
	if len(clientHeaders) > 0 && clientHeaders[0] != nil {
		ginHeaders = clientHeaders[0]
	} else if ginCtx, ok := r.Context().Value("gin").(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
		ginHeaders = ginCtx.Request.Header
	}
	applyCodexHeadersWithSIWC(r, auth, codexUseSIWCForRequest(r.Context(), auth, ginHeaders), token, stream, cfg, ginHeaders)
}

// applyModelHeaderOverrides forces models.json config.override_header onto upstream headers.
func applyModelHeaderOverrides(headers http.Header, modelName string) {
	if headers == nil {
		return
	}
	overrides := registry.ModelOverrideHeaders(modelName)
	if len(overrides) == 0 {
		return
	}
	for key, value := range overrides {
		headers.Set(key, value)
	}
	if strings.Contains(headers.Get("User-Agent"), "Mac OS") && codexSessionHeaderValue(headers) == "" {
		headers.Set("Session_id", uuid.NewString())
	}
}

// applyCodexDirectImageHeaders sets Codex upstream headers for direct /images/* calls.
// Downstream client User-Agent values are not forwarded to reduce Cloudflare 1010 blocks.
func applyCodexDirectImageHeaders(r *http.Request, auth *cliproxyauth.Auth, token string, stream bool, cfg *config.Config, clientHeaders ...http.Header) {
	var ginHeaders http.Header
	if len(clientHeaders) > 0 && clientHeaders[0] != nil {
		ginHeaders = clientHeaders[0].Clone()
		ginHeaders.Del("User-Agent")
	} else if ginCtx, ok := r.Context().Value("gin").(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
		ginHeaders = ginCtx.Request.Header.Clone()
		ginHeaders.Del("User-Agent")
	}
	applyCodexHeadersWithSIWC(r, auth, false, token, stream, cfg, ginHeaders)
}

// applyCodexImageCompatibilityHeaders runs after identity mapping and before
// model overrides. It changes headers only, never the final media payload.
func applyCodexImageCompatibilityHeaders(r *http.Request, auth *cliproxyauth.Auth, cfg *config.Config, state *codexIdentityConfuseState, source http.Header) {
	if cfg == nil || !strings.EqualFold(strings.TrimSpace(cfg.Codex.HTTPTLSProfile), "linux") || auth == nil || codexAuthUsesAPIKey(auth) || codexUseSIWCForRequest(r.Context(), auth, source) {
		return
	}
	if source == nil {
		if g, ok := r.Context().Value("gin").(*gin.Context); ok && g != nil && g.Request != nil {
			source = g.Request.Header
		}
	}
	applyCodexAccountHeaders(r.Context(), r.Header, cfg, auth, source)
	accept := source.Get("Accept")
	if accept == "" {
		accept = "*/*"
	}
	if override := codexOperatorHeaderValue(r.Context(), auth, source, "Accept"); override != "" {
		accept = override
	}
	r.Header.Set("Accept", accept)
	if r.Header.Get("Version") == "" || (!isCodexCloakingDisabled(cfg, auth) && strings.TrimSpace(cfg.CodexHeaderDefaults.Version) == "" && codexOperatorHeaderValue(r.Context(), auth, source, "Version") == "") {
		// Derive from the effective identity when no explicit version is configured.
		for _, part := range strings.Fields(r.Header.Get("User-Agent")) {
			if strings.HasPrefix(part, "codex") {
				if _, version, ok := strings.Cut(part, "/"); ok {
					r.Header.Set("Version", version)
					break
				}
			}
		}
	}
	const imageTurnHeader = "X-Codex-Image-Turn-Id"
	turn := strings.TrimSpace(ojson.Get(r.Header.Get("X-Codex-Turn-Metadata"), "turn_id").String())
	if turn == "" {
		turn = strings.TrimSpace(source.Get(imageTurnHeader))
		if state != nil && turn != "" {
			turn = codexAccountIdentityUUID(state.accountIdentity, "turn", turn)
			if state.enabled {
				turn = state.confuseTurnID(turn)
			}
		}
	}
	if turn == "" && state != nil && state.convergence.enabled {
		turn = state.convergence.turnID
	}
	if turn == "" {
		turn = uuid.Must(uuid.NewV7()).String()
	}
	if override := codexOperatorHeaderValue(r.Context(), auth, source, imageTurnHeader); override != "" {
		turn = override
	}
	r.Header.Set(imageTurnHeader, turn)
}

func codexSIWCUserAgent(cfg *config.Config) string {
	agentName := config.DefaultSignInWithChatGPTIdentifier
	if cfg != nil {
		agentName = cfg.Codex.EffectiveSignInWithChatGPTIdentifier()
	}
	return fmt.Sprintf("%s (%s; %s)", agentName, runtime.GOOS, runtime.GOARCH)
}

func applyCodexSIWCRequestBodyRules(body []byte, hasExplicitInstructions bool, opts ...cliproxyexecutor.Options) []byte {
	body = helps.SetBoolIfDifferent(body, "store", false)
	body = helps.SetBoolIfDifferent(body, "stream", true)
	return stripCodexSIWCUnsupportedFields(body, hasExplicitInstructions, opts...)
}

func applyCodexSIWCWebsocketBodyRules(body []byte, hasExplicitInstructions bool, opts ...cliproxyexecutor.Options) []byte {
	body = helps.SetBoolIfDifferent(body, "store", false)
	body, _ = ojson.DeleteBytes(body, "stream")
	body, _ = ojson.DeleteBytes(body, "background")
	return stripCodexSIWCUnsupportedFields(body, hasExplicitInstructions, opts...)
}

func applyCodexSIWCCompactBodyRules(body []byte, hasExplicitInstructions bool, opts ...cliproxyexecutor.Options) []byte {
	body, _ = ojson.DeleteBytes(body, "stream")
	body, _ = ojson.DeleteBytes(body, "store")
	return stripCodexSIWCUnsupportedFields(body, hasExplicitInstructions, opts...)
}

func stripCodexSIWCUnsupportedFields(body []byte, hasExplicitInstructions bool, opts ...cliproxyexecutor.Options) []byte {
	// SIWC subscriptions reject configuration_update input items, so degrade
	// them to the classic top-level reasoning.effort and accept the cache loss.
	// An explicit model thinking suffix is an operator override: the thinking
	// layer has already resolved it into the top-level effort, so only the
	// unsupported items are removed in that case.
	body = thinking.CollapseConfigurationUpdates(body, len(opts) > 0 && opts[0].KeepTopLevelEffort)
	body, _ = ojson.DeleteBytes(body, "prompt_cache_retention")
	body, _ = ojson.DeleteBytes(body, "prompt_cache_options")
	body, _ = ojson.DeleteBytes(body, "max_completion_tokens")
	body, _ = ojson.DeleteBytes(body, "temperature")
	if !hasExplicitInstructions {
		if inst := ojson.GetBytes(body, "instructions"); inst.Exists() && strings.TrimSpace(inst.String()) == "" {
			body, _ = ojson.DeleteBytes(body, "instructions")
		}
	}
	return body
}

func applyCodexHeadersFromSources(r *http.Request, auth *cliproxyauth.Auth, token string, stream bool, cfg *config.Config, ginHeaders http.Header) {
	applyCodexHeadersWithSIWC(r, auth, codexUseSIWCForRequest(r.Context(), auth, ginHeaders), token, stream, cfg, ginHeaders)
}

func applyCodexHeadersWithSIWC(r *http.Request, auth *cliproxyauth.Auth, useSIWC bool, token string, stream bool, cfg *config.Config, ginHeaders http.Header) {
	defer helps.GuardCodexTurnStateHeaders(r.Context(), r.Header)
	r.Header.Set("Content-Type", "application/json")
	if strings.TrimSpace(token) != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	} else {
		r.Header.Del("Authorization")
	}

	if useSIWC {
		if sessionID := strings.TrimSpace(r.Header.Get("Session-Id")); sessionID != "" {
			r.Header.Del("Session-Id")
			setHeaderCasePreserved(r.Header, "session_id", sessionID)
			r.Header.Set("X-Client-Request-Id", sessionID)
		}
		r.Header.Set("User-Agent", codexSIWCUserAgent(cfg))
		if stream {
			r.Header.Set("Accept", "text/event-stream")
		} else {
			r.Header.Set("Accept", "application/json")
		}
		var attrs map[string]string
		if auth != nil {
			attrs = auth.Attributes
		}
		if auth != nil && !codexAuthUsesAPIKey(auth) {
			helps.StripCodexTraceContextHeaders(r.Header)
		}
		util.ApplyCustomHeadersFromAttrs(r, attrs, ginHeaders)
		return
	}

	if ginHeaders != nil && ginHeaders.Get("X-Codex-Beta-Features") != "" {
		r.Header.Set("X-Codex-Beta-Features", ginHeaders.Get("X-Codex-Beta-Features"))
	}
	misc.EnsureHeader(r.Header, ginHeaders, "Version", "")
	misc.EnsureHeader(r.Header, ginHeaders, "X-Codex-Turn-Metadata", "")
	misc.EnsureHeader(r.Header, ginHeaders, "X-Codex-Turn-State", "")
	misc.EnsureHeader(r.Header, ginHeaders, "X-Client-Request-Id", "")
	misc.EnsureHeader(r.Header, ginHeaders, "X-Codex-Window-Id", "")
	misc.EnsureHeader(r.Header, ginHeaders, "Thread-Id", "")
	misc.EnsureHeader(r.Header, ginHeaders, "X-Codex-Parent-Thread-Id", "")
	misc.EnsureHeader(r.Header, ginHeaders, "X-OpenAI-Subagent", "")
	if auth != nil && !codexAuthUsesAPIKey(auth) {
		misc.EnsureHeader(r.Header, ginHeaders, "X-OpenAI-Memgen-Request", "")
	}
	misc.EnsureHeader(r.Header, ginHeaders, "X-Codex-Guardian", "")
	misc.EnsureHeader(r.Header, ginHeaders, "Session-Id", "")
	misc.EnsureHeader(r.Header, ginHeaders, "X-Openai-Internal-Codex-Responses-Lite", "")

	cfgUserAgent, _ := codexHeaderDefaults(cfg, auth)
	ensureHeaderWithConfigPrecedence(r.Header, ginHeaders, "User-Agent", cfgUserAgent, codexUserAgent)

	if stream {
		r.Header.Set("Accept", "text/event-stream")
	} else {
		r.Header.Set("Accept", "application/json")
	}

	isAPIKey := codexAuthUsesAPIKey(auth)
	if originator := strings.TrimSpace(ginHeaders.Get("Originator")); originator != "" {
		r.Header.Set("Originator", originator)
	} else if !isAPIKey {
		r.Header.Set("Originator", codexOriginator)
	}
	if !isAPIKey {
		if auth != nil && auth.Metadata != nil {
			if accountID, ok := auth.Metadata["account_id"].(string); ok {
				r.Header.Set("Chatgpt-Account-Id", accountID)
			}
		}
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	if cfg != nil && auth != nil && !isAPIKey {
		if version := strings.TrimSpace(cfg.CodexHeaderDefaults.Version); version != "" {
			r.Header.Set("Version", version)
		}
	}
	if auth != nil && !codexAuthUsesAPIKey(auth) {
		helps.StripCodexTraceContextHeaders(r.Header)
	}
	util.ApplyCustomHeadersFromAttrs(r, attrs, ginHeaders)
	applyCodexCloakingHeaders(r.Header, cfg, auth)
	applyCodexAccountHeaders(r.Context(), r.Header, cfg, auth, ginHeaders, useSIWC)
}

const codexRoutingHintHeader = "X-Codex-Routing-Hint"

// applyCodexRoutingHint sends the routing hint native Codex attaches to
// ordinary ChatGPT-backend Responses requests: "model=<slug>" plus
// ";tier=<service_tier>" when the body requests a tier (openai/codex rust-v0.155.0,
// codex-rs/core/src/client.rs build_routing_hint_header). Without it, a
// translated request carries service_tier=priority only in the body. Whether
// the backend needs the header to grant priority is undocumented.
//
// The model is the resolved model written to the upstream body, while the tier
// is read from the final body so payload rules cannot make the hint stale. A
// hint forwarded by a native client names its original model and is replaced.
// Operator configuration keeps precedence: when an auth "header:" rule for the
// hint resolves to a value (static, or a "$Header" reference the request
// carries), that value is sent, and callers apply models.json override_header
// afterwards. A rule that resolves to nothing falls back to the derived hint.
// API-key requests are not touched, matching native Codex, which sends no hint
// to API-key providers.
func applyCodexRoutingHint(ctx context.Context, headers http.Header, auth *cliproxyauth.Auth, baseModel string, upstreamBody []byte, clientHeaders http.Header) {
	if codexAuthUsesAPIKey(auth) || codexUseSIWCForRequest(ctx, auth, clientHeaders) {
		return
	}
	deleteHeaderCaseInsensitive(headers, codexRoutingHintHeader)
	if operatorHint := codexOperatorHeaderValue(ctx, auth, clientHeaders, codexRoutingHintHeader); operatorHint != "" {
		headers.Set(codexRoutingHintHeader, operatorHint)
		return
	}
	// Native Codex 0.162 omits this hint for both Guardian request types.
	switch headers.Get("X-Codex-Guardian") {
	case "reviewer", "classifier":
		return
	}
	model := strings.TrimSpace(baseModel)
	if model == "" {
		return
	}
	hint := "model=" + model
	if tier := ojson.GetBytes(upstreamBody, "service_tier"); tier.Type == ojson.String {
		if value := strings.TrimSpace(tier.String()); value != "" {
			hint += ";tier=" + value
		}
	}
	headers.Set(codexRoutingHintHeader, hint)
}

// codexOperatorHeaderValue returns the value the auth's "header:" rules
// resolve to for name, using the same resolver that applied them to the
// request, so dynamic references that resolve to nothing report "".
func codexOperatorHeaderValue(ctx context.Context, auth *cliproxyauth.Auth, clientHeaders http.Header, name string) string {
	if auth == nil || len(auth.Attributes) == 0 {
		return ""
	}
	resolved := (&http.Request{Header: http.Header{}}).WithContext(ctx)
	util.ApplyCustomHeadersFromAttrs(resolved, auth.Attributes, clientHeaders)
	return strings.TrimSpace(resolved.Header.Get(name))
}

func isCodexCloakingDisabled(cfg *config.Config, auth *cliproxyauth.Auth) bool {
	if auth != nil && auth.AuthKind() == cliproxyauth.AuthKindAPIKey {
		cfg = cfg.ForAPIKey()
	}
	if auth != nil && len(auth.Attributes) > 0 {
		if val, ok := auth.Attributes[cliproxyauth.AttributeCodexDisableCloaking]; ok {
			if parsed, errParse := strconv.ParseBool(strings.TrimSpace(val)); errParse == nil {
				return parsed
			}
		}
	}
	if entry := resolveCodexKeyConfig(cfg, auth); entry != nil && entry.DisableCodexCloaking != nil {
		return *entry.DisableCodexCloaking
	}
	if cfg != nil && cfg.Codex.DisableCodexCloaking {
		return true
	}
	return false
}

func applyCodexCloakingHeaders(headers http.Header, cfg *config.Config, auth *cliproxyauth.Auth) {
	if headers == nil || cfg == nil || isCodexCloakingDisabled(cfg, auth) {
		return
	}
	userAgent, _ := codexHeaderDefaults(cfg, auth)
	if userAgent == "" {
		userAgent = codexUserAgent
	}
	headers.Set("User-Agent", userAgent)
	headers.Set("Originator", codexOriginator)
}

func normalizeCodexInstructions(body []byte, nativeRequest ...bool) []byte {
	if len(nativeRequest) > 0 && nativeRequest[0] {
		return body
	}
	instructions := ojson.GetBytes(body, "instructions")
	if !instructions.Exists() || instructions.Type == ojson.Null {
		body, _ = ojson.SetBytes(body, "instructions", "")
	}
	return body
}

func isCodexResponsesLiteModel(model string) bool {
	return helps.CodexModelUsesResponsesLite(model)
}

var imageGenToolJSON = []byte(`{"type":"image_generation","output_format":"png"}`)
var imageGenToolArrayJSON = []byte(`[{"type":"image_generation","output_format":"png"}]`)

func isCodexFreePlanAuth(auth *cliproxyauth.Auth) bool {
	if auth == nil || auth.Attributes == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(auth.Attributes["plan_type"]), "free")
}

func isImageGenerationFunctionTool(tool ojson.Result) bool {
	switch tool.Get("type").String() {
	case "function":
		return tool.Get("name").String() == "image_gen.imagegen"
	case "namespace":
		if tool.Get("name").String() != "image_gen" {
			return false
		}
		tools := tool.Get("tools")
		if !tools.IsArray() {
			return false
		}
		for _, nestedTool := range tools.Array() {
			if nestedTool.Get("type").String() == "function" && nestedTool.Get("name").String() == "imagegen" {
				return true
			}
		}
	}
	return false
}

func ensureImageGenerationTool(body []byte, baseModel string, auth *cliproxyauth.Auth, headers http.Header) []byte {
	doc := ojson.NewDoc(body)
	if !ensureImageGenerationToolDoc(doc, baseModel, auth, headers) {
		return body
	}
	return doc.Bytes()
}

func codexRequestHasReasoningForHostedImageDoc(doc *ojson.Doc, model string) bool {
	return helps.CodexRequestHasReasoningForHostedImageDoc(doc, model)
}

func isCodexResponsesLiteRequestForAuthDoc(doc *ojson.Doc, modelName string, auth *cliproxyauth.Auth, headers http.Header) bool {
	return helps.CodexResponsesLiteRequestForAuthDoc(doc, modelName, auth, headers)
}

func codexToolsAlreadyContainImageGenDoc(doc *ojson.Doc) bool {
	if doc == nil {
		return false
	}
	tools := doc.Get("tools")
	if !tools.Exists() || !tools.IsArray() {
		return false
	}
	for _, t := range tools.Array() {
		if t.Get("type").String() == "image_generation" || isImageGenerationFunctionTool(t) {
			return true
		}
	}
	return false
}

func appendImageGenerationToolDoc(doc *ojson.Doc) bool {
	if doc == nil {
		return false
	}
	tools := doc.Get("tools")
	if !tools.Exists() || !tools.IsArray() {
		return doc.SetRawBytes("tools", imageGenToolArrayJSON) == nil
	}
	return doc.SetRawBytes("tools.-1", imageGenToolJSON) == nil
}

func ensureImageGenerationToolDoc(doc *ojson.Doc, baseModel string, auth *cliproxyauth.Auth, headers http.Header) bool {
	if doc == nil || isCodexFreePlanAuth(auth) || codexToolsAlreadyContainImageGenDoc(doc) {
		return false
	}
	modelName := strings.TrimSpace(thinking.ParseSuffix(baseModel).ModelName)
	if modelName == "" {
		modelName = strings.TrimSpace(thinking.ParseSuffix(doc.Get("model").String()).ModelName)
	}
	if isCodexResponsesLiteRequestForAuthDoc(doc, modelName, auth, headers) {
		return false
	}
	if isCodexResponsesLiteModel(modelName) && codexRequestHasReasoningForHostedImageDoc(doc, baseModel) {
		return false
	}
	if strings.HasSuffix(strings.ToLower(modelName), "spark") {
		return false
	}
	return appendImageGenerationToolDoc(doc)
}

// normalizeCodexImageGenerationBody normalizes hosted tools before identity and
// final payload rules, preserving original bytes when no change is needed.
func normalizeCodexImageGenerationBody(body []byte) []byte {
	doc := ojson.NewDoc(body)
	if normalizeCodexImageGenerationToolsDoc(doc) {
		return doc.Bytes()
	}
	return body
}

func resolveCodexGPTImage2BaseModel(cfg *config.Config) string {
	if cfg == nil {
		return codexOpenAIImagesMainModel
	}
	model := strings.TrimSpace(cfg.GPTImage2BaseModel)
	if model == "" {
		return codexOpenAIImagesMainModel
	}
	if !strings.HasPrefix(strings.ToLower(model), "gpt-") || isCodexResponsesLiteModel(model) {
		return codexOpenAIImagesMainModel
	}
	return model
}

// normalizeCodexImageGenerationToolsDoc strips parameters rejected by the
// hosted image_generation tool on Codex /responses (which binds to
// gpt-image-2-codex and rejects the complexity parameter).
func normalizeCodexImageGenerationToolsDoc(doc *ojson.Doc) bool {
	if doc == nil {
		return false
	}
	changed := false
	stripAtPath := func(toolsPath string) {
		tools := doc.Get(toolsPath)
		if !tools.IsArray() {
			return
		}
		for idx, tool := range tools.Array() {
			if strings.TrimSpace(tool.Get("type").String()) != "image_generation" {
				continue
			}
			if tool.Get("complexity").Exists() {
				if doc.Delete(fmt.Sprintf("%s.%d.complexity", toolsPath, idx)) == nil {
					changed = true
				}
			}
		}
	}
	stripAtPath("tools")
	input := doc.Get("input")
	if input.IsArray() {
		for idx, item := range input.Array() {
			if strings.TrimSpace(item.Get("type").String()) != "additional_tools" {
				continue
			}
			stripAtPath(fmt.Sprintf("input.%d.tools", idx))
		}
	}
	return changed
}

func normalizeCodexParallelToolCalls(body []byte, headers http.Header) []byte {
	if util.IsCodexResponsesLiteRequest(body, headers) {
		body = helps.SetBoolIfDifferent(body, "parallel_tool_calls", false)
		return body
	}
	return normalizeCodexParallelToolCallsForTools(body)
}

func normalizeCodexParallelToolCallsForTools(body []byte) []byte {
	if !ojson.GetBytes(body, "parallel_tool_calls").Exists() {
		return body
	}

	tools := ojson.GetBytes(body, "tools")
	hasTools := tools.Exists() && tools.IsArray() && len(tools.Array()) > 0
	if hasTools {
		return body
	}

	body, _ = ojson.DeleteBytes(body, "parallel_tool_calls")
	return body
}

func publishCodexImageToolUsage(ctx context.Context, reporter *helps.UsageReporter, body []byte, completedData []byte) {
	publishCodexImageToolUsageWithOutputItems(ctx, reporter, body, completedData, nil, nil)
}

func publishCodexImageToolUsageWithOutputItems(ctx context.Context, reporter *helps.UsageReporter, body []byte, completedData []byte, itemsByIndex map[int64][]byte, fallback [][]byte) {
	detail, ok := helps.ParseCodexImageToolUsageWithOutputItems(body, completedData, itemsByIndex, fallback)
	if !ok {
		return
	}
	reporter.EnsurePublished(ctx)
	reporter.PublishAdditionalModel(ctx, codexImageGenerationToolModel(body), detail)
}

func codexImageGenerationToolModel(body []byte) string {
	tools := ojson.GetBytes(body, "tools")
	if tools.IsArray() {
		for _, tool := range tools.Array() {
			if tool.Get("type").String() != "image_generation" {
				continue
			}
			if model := strings.TrimSpace(tool.Get("model").String()); model != "" {
				return model
			}
			break
		}
	}
	return codexDefaultImageToolModel
}

// codexTurnStateContext uses original client identities, before account mapping.
func codexTurnStateContext(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) context.Context {
	owner := ""
	if auth != nil && !codexAuthUsesAPIKey(auth) && !codexUseSIWCForRequest(ctx, auth, opts.Headers) {
		owner = codexCredentialIdentitySource(auth)
	}
	body := req.Payload
	if len(opts.OriginalRequest) > 0 {
		body = opts.OriginalRequest
	}
	headers := opts.Headers
	if headers == nil && ctx != nil {
		if ginCtx, ok := ctx.Value("gin").(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
			headers = ginCtx.Request.Header
		}
	}
	session := executionSessionIDFromOptions(opts)
	if session == "" {
		session = helps.ProviderSessionUUID("codex", req.Metadata)
	}
	if session == "" {
		session = helps.ProviderSessionUUID("codex", opts.Metadata)
	}
	return helps.WithCodexTurnState(ctx, owner, headers, body, session)
}

// applyCodexAccountHeaders restores persisted OAuth account identity after cloaking.
// Assignment and release discovery belong to the account operations plugin.
func applyCodexAccountHeaders(ctx context.Context, headers http.Header, cfg *config.Config, auth *cliproxyauth.Auth, clientHeaders http.Header, selectedSIWC ...bool) {
	useSIWC := codexUseSIWCForRequest(ctx, auth, clientHeaders)
	if len(selectedSIWC) > 0 {
		useSIWC = selectedSIWC[0]
	}
	if headers == nil || cfg == nil || auth == nil || codexAuthUsesAPIKey(auth) || useSIWC {
		return
	}
	for _, name := range []string{"User-Agent", "Version", "Originator"} {
		if value := codexOperatorHeaderValue(ctx, auth, clientHeaders, name); value != "" {
			headers.Set(name, value)
		}
	}
}

func applyCodexSIWCRequestBodyRulesDoc(doc *ojson.Doc, hasExplicitInstructions bool, opts ...cliproxyexecutor.Options) bool {
	if doc == nil {
		return false
	}
	changed := helps.SetBoolIfDifferentDoc(doc, "store", false)
	if helps.SetBoolIfDifferentDoc(doc, "stream", true) {
		changed = true
	}
	if stripCodexSIWCUnsupportedFieldsDoc(doc, hasExplicitInstructions, opts...) {
		changed = true
	}
	return changed
}

func applyCodexSIWCWebsocketBodyRulesDoc(doc *ojson.Doc, hasExplicitInstructions bool, opts ...cliproxyexecutor.Options) bool {
	if doc == nil {
		return false
	}
	changed := helps.SetBoolIfDifferentDoc(doc, "store", false)
	if doc.Delete("stream") == nil {
		changed = true
	}
	if doc.Delete("background") == nil {
		changed = true
	}
	if stripCodexSIWCUnsupportedFieldsDoc(doc, hasExplicitInstructions, opts...) {
		changed = true
	}
	return changed
}

func applyCodexSIWCCompactBodyRulesDoc(doc *ojson.Doc, hasExplicitInstructions bool, opts ...cliproxyexecutor.Options) bool {
	if doc == nil {
		return false
	}
	changed := false
	if doc.Delete("stream") == nil {
		changed = true
	}
	if doc.Delete("store") == nil {
		changed = true
	}
	if stripCodexSIWCUnsupportedFieldsDoc(doc, hasExplicitInstructions, opts...) {
		changed = true
	}
	return changed
}

func stripCodexSIWCUnsupportedFieldsDoc(doc *ojson.Doc, hasExplicitInstructions bool, opts ...cliproxyexecutor.Options) bool {
	if doc == nil {
		return false
	}
	// SIWC subscriptions reject configuration_update input items, so degrade
	// them to the classic top-level reasoning.effort and accept the cache loss.
	// An explicit model thinking suffix is an operator override: the thinking
	// layer has already resolved it into the top-level effort, so only the
	// unsupported items are removed in that case.
	thinking.CollapseConfigurationUpdatesDoc(doc, len(opts) > 0 && opts[0].KeepTopLevelEffort)
	_ = doc.Delete("prompt_cache_retention")
	_ = doc.Delete("prompt_cache_options")
	_ = doc.Delete("max_output_tokens")
	_ = doc.Delete("max_completion_tokens")
	_ = doc.Delete("temperature")
	if !hasExplicitInstructions {
		if inst := doc.Get("instructions"); inst.Exists() && strings.TrimSpace(inst.String()) == "" {
			_ = doc.Delete("instructions")
		}
	}
	return true
}

func normalizeCodexInstructionsDoc(doc *ojson.Doc, nativeRequest ...bool) bool {
	if doc == nil || (len(nativeRequest) > 0 && nativeRequest[0]) {
		return false
	}
	instructions := doc.Get("instructions")
	if !instructions.Exists() || instructions.Type == ojson.Null {
		return doc.Set("instructions", "") == nil
	}
	return false
}

func isCodexResponsesLiteRequestDoc(doc *ojson.Doc, headers http.Header) bool {
	return helps.CodexResponsesLiteRequestDoc(doc, headers)
}

func normalizeCodexParallelToolCallsDoc(doc *ojson.Doc, headers http.Header) bool {
	if doc == nil {
		return false
	}
	if isCodexResponsesLiteRequestDoc(doc, headers) {
		return helps.SetBoolIfDifferentDoc(doc, "parallel_tool_calls", false)
	}
	return normalizeCodexParallelToolCallsForToolsDoc(doc)
}

func normalizeCodexParallelToolCallsForToolsDoc(doc *ojson.Doc) bool {
	if doc == nil || !doc.Get("parallel_tool_calls").Exists() {
		return false
	}

	tools := doc.Get("tools")
	hasTools := tools.Exists() && tools.IsArray() && len(tools.Array()) > 0
	if hasTools {
		return false
	}

	return doc.Delete("parallel_tool_calls") == nil
}
