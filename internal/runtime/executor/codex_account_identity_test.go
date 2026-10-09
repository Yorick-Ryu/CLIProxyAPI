package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCodexAccountIdentityPreservesSessionAndTurnLineage(t *testing.T) {
	auth := &cliproxyauth.Auth{ID: "test", Provider: "codex", Metadata: map[string]any{"account_id": "test-account"}}
	ctx := codexAccountIdentityTestContext("caller")
	mapRequest := func(thread, turn, root, parent string) ([]byte, http.Header) {
		t.Helper()
		meta := map[string]string{"session_id": "root-thread", "thread_id": thread, "turn_id": turn, "root_turn_id": root, "parent_turn_id": parent, "parent_thread_id": "root-thread", "forked_from_thread_id": "root-thread", "window_id": thread + ":3"}
		raw, _ := json.Marshal(meta)
		body, _ := json.Marshal(map[string]any{"prompt_cache_key": "root-thread", "client_metadata": map[string]string{"session_id": "root-thread", "thread_id": thread, "turn_id": turn, "root_turn_id": root, "parent_turn_id": parent, "x-codex-window-id": thread + ":3", "x-codex-turn-metadata": string(raw)}})
		mapped, state := applyCodexAccountIdentityBody(ctx, auth, body)
		headers := http.Header{"Session-Id": {"root-thread"}, "Thread-Id": {thread}, "X-Client-Request-Id": {thread}, "X-Codex-Window-Id": {thread + ":3"}, "X-Codex-Turn-Metadata": {string(raw)}}
		applyCodexAccountIdentityHeaders(headers, &state)
		return mapped, headers
	}
	parent, headers := mapRequest("root-thread", "root-turn", "root-turn", "root-turn")
	session := headers.Get("Session-Id")
	if session != headers.Get("Thread-Id") || session != headers.Get("X-Client-Request-Id") || headers.Get("X-Codex-Window-Id") != session+":3" {
		t.Fatal("session/thread/request/window relationship lost")
	}
	if session != gjson.GetBytes(parent, "prompt_cache_key").String() {
		t.Fatal("cache key differs from session")
	}
	turn := gjson.GetBytes(parent, "client_metadata.turn_id").String()
	if turn == "root-turn" || turn != gjson.GetBytes(parent, "client_metadata.root_turn_id").String() {
		t.Fatal("root turn relation lost")
	}
	child, childHeaders := mapRequest("child-thread", "child-turn", "root-turn", "root-turn")
	childMeta := gjson.Parse(childHeaders.Get("X-Codex-Turn-Metadata"))
	if childHeaders.Get("Session-Id") != session || childHeaders.Get("Thread-Id") == session {
		t.Fatal("child session/thread distinction lost")
	}
	if childMeta.Get("root_turn_id").String() != turn || childMeta.Get("parent_turn_id").String() != turn || childMeta.Get("turn_id").String() == turn {
		t.Fatal("child turn lineage lost")
	}
	if childMeta.Get("parent_thread_id").String() != session || childMeta.Get("forked_from_thread_id").String() != session {
		t.Fatal("parent thread lineage lost")
	}
	if childHeaders.Get("X-Codex-Turn-Metadata") != gjson.GetBytes(child, "client_metadata.x-codex-turn-metadata").String() {
		t.Fatal("header/body metadata differs")
	}
}

func TestCodexAccountIdentityPreservesThreadRelationsAndWindowGeneration(t *testing.T) {
	auth := &cliproxyauth.Auth{ID: "test", Provider: "codex", Metadata: map[string]any{"account_id": "test"}}
	for _, generation := range []string{"0", "2", "1024"} {
		body := []byte(fmt.Sprintf(`{"client_metadata":{"thread_id":"thread-a","x-client-request-id":"thread-a","x-codex-window-id":"thread-a:%s","x-codex-turn-metadata":"{\"thread_id\":\"thread-a\",\"x-client-request-id\":\"thread-a\",\"window_id\":\"thread-a:%s\"}"}}`, generation, generation))
		mapped, state := applyCodexAccountIdentityBody(codexAccountIdentityTestContext("caller"), auth, body)
		thread := gjson.GetBytes(mapped, "client_metadata.thread_id").String()
		if thread == "" || thread == "thread-a" {
			t.Fatal("thread identity must be scoped")
		}
		metadata := gjson.Get(gjson.GetBytes(mapped, "client_metadata.x-codex-turn-metadata").String(), "@this")
		if gjson.GetBytes(mapped, "client_metadata.x-client-request-id").String() != thread || metadata.Get("x-client-request-id").String() != thread {
			t.Fatal("request identity lost its relationship with the thread")
		}
		if gjson.GetBytes(mapped, "client_metadata.x-codex-window-id").String() != thread+":"+generation || metadata.Get("window_id").String() != thread+":"+generation {
			t.Fatal("window generation or thread prefix changed")
		}
		headers := http.Header{"Thread-Id": {"thread-a"}, "X-Client-Request-Id": {"independent-request"}, "X-Codex-Window-Id": {"thread-a:" + generation}}
		applyCodexAccountIdentityHeaders(headers, &state)
		if headers.Get("X-Client-Request-Id") == thread || headers.Get("X-Codex-Window-Id") != thread+":"+generation {
			t.Fatal("distinct input identities must remain distinct, with window syntax preserved")
		}
	}
}

func codexAccountIdentityTestContext(apiKey string) context.Context {
	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	ginContext.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ginContext.Set("userApiKey", apiKey)
	return context.WithValue(context.Background(), "gin", ginContext)
}

func TestCodexGuardianAndForkOnWire(t *testing.T) {
	for _, transport := range []string{"http", "ws"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream%v", transport, stream), func(t *testing.T) {
				type capture struct {
					headers http.Header
					body    []byte
				}
				captured := make(chan capture, 1)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					completed := []byte(`{"type":"response.completed","response":{"id":"resp_fixture","status":"completed","output":[]}}`)
					if transport == "ws" {
						conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
						if err != nil {
							t.Error(err)
							return
						}
						defer conn.Close()
						_, body, err := conn.ReadMessage()
						if err != nil {
							t.Error(err)
							return
						}
						captured <- capture{r.Header.Clone(), body}
						_ = conn.WriteMessage(websocket.TextMessage, completed)
					} else {
						body, err := io.ReadAll(r.Body)
						if err != nil {
							t.Error(err)
							return
						}
						captured <- capture{r.Header.Clone(), body}
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = fmt.Fprintf(w, "data: %s\n\n", completed)
					}
				}))
				defer server.Close()
				cfg := &config.Config{Codex: config.CodexConfig{IdentityConvergence: true}}
				var executor cliproxyauth.ProviderExecutor = NewCodexExecutor(cfg)
				if transport == "ws" {
					executor = NewCodexWebsocketsExecutor(cfg)
				}
				auth := codexOAuthTestAuth(server.URL)
				auth.ID = "guardian-fork-wire"
				ctx := codexAccountIdentityTestContext("fixture-caller")
				var sharedCache string
				// The fork arrives first: no prior parent request or process cache is needed.
				for _, tc := range []struct{ guardian, thread, wantHint string }{
					{"reviewer", "fork", ""},
					{"", "parent", "model=gpt-5.5"},
					{"classifier", "another-fork", ""},
				} {
					body, err := json.Marshal(map[string]any{"model": "gpt-5.5", "input": []any{}, "prompt_cache_key": "parent", "client_metadata": map[string]string{"session_id": tc.thread, "thread_id": tc.thread, "parent_response_id": "resp_upstream_opaque"}})
					if err != nil {
						t.Fatal(err)
					}
					headers := http.Header{"Session-Id": {"parent"}, "Thread-Id": {tc.thread}}
					if tc.guardian != "" {
						headers.Set("X-Codex-Guardian", tc.guardian)
					}
					req := cliproxyexecutor.Request{Model: "gpt-5.5", Payload: body}
					opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatCodex, Headers: headers, Stream: stream}
					if stream {
						result, err := executor.ExecuteStream(ctx, auth, req, opts)
						if err != nil {
							t.Fatal(err)
						}
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								t.Fatal(chunk.Err)
							}
						}
					} else if _, err := executor.Execute(ctx, auth, req, opts); err != nil {
						t.Fatal(err)
					}
					got := <-captured
					cache := gjson.GetBytes(got.body, "prompt_cache_key").String()
					if sharedCache == "" {
						sharedCache = cache
					}
					if cache == "" || cache == "parent" || cache != sharedCache || got.headers.Get("Session-Id") != cache {
						t.Fatal("parent and forks lost their shared cache routing identity")
					}
					thread := gjson.GetBytes(got.body, "client_metadata.thread_id").String()
					if got.headers.Get("Thread-Id") != thread || gjson.GetBytes(got.body, "client_metadata.session_id").String() != thread || (thread == cache) != (tc.thread == "parent") {
						t.Fatal("fork must retain its own session/thread identity")
					}
					if gjson.GetBytes(got.body, "client_metadata.parent_response_id").String() != "resp_upstream_opaque" {
						t.Fatal("upstream response reference must not be remapped")
					}
					if got.headers.Get("X-Codex-Guardian") != tc.guardian {
						t.Fatal("guardian header lost or synthesized")
					}
					if got.headers.Get(codexRoutingHintHeader) != tc.wantHint {
						t.Fatalf("guardian %q routing hint = %q, want %q", tc.guardian, got.headers.Get(codexRoutingHintHeader), tc.wantHint)
					}
					if tc.wantHint == "" && got.headers.Values(codexRoutingHintHeader) != nil {
						t.Fatalf("guardian %q must omit the routing hint header", tc.guardian)
					}
				}
			})
		}
	}
}

func TestCodexAccountIdentityScopesByCallerCredentialAndRawIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"prompt_cache_key":"client-session-a","client_metadata":{"session_id":"client-session-a","thread_id":"client-thread-a","x-codex-installation-id":"client-install-a","x-codex-window-id":"client-window-a","x-codex-turn-metadata":"{\"installation_id\":\"client-install-a\",\"session_id\":\"client-session-a\",\"thread_id\":\"client-thread-a\",\"turn_id\":\"client-turn-a\",\"window_id\":\"client-window-a\"}"}}`)
	auth := &cliproxyauth.Auth{
		ID:       "local-auth-a",
		Provider: "codex",
		Metadata: map[string]any{"account_id": "upstream-account-a"},
	}

	firstBody, first := applyCodexAccountIdentityBody(codexAccountIdentityTestContext("caller-key-a"), auth, body)
	secondBody, second := applyCodexAccountIdentityBody(codexAccountIdentityTestContext("caller-key-a"), auth, body)
	if !first.enabled || !second.enabled || string(firstBody) != string(secondBody) {
		t.Fatal("the same caller, credential, and raw identity must map deterministically")
	}
	mappedSession := gjson.GetBytes(firstBody, "client_metadata.session_id").String()
	if mappedSession == "" || mappedSession == "client-session-a" {
		t.Fatalf("mapped session = %q, want a non-empty anonymous value", mappedSession)
	}
	if got := gjson.GetBytes(firstBody, "prompt_cache_key").String(); got != mappedSession {
		t.Fatalf("mapped prompt_cache_key = %q, want mapped session %q", got, mappedSession)
	}
	if got := gjson.Get(gjson.GetBytes(firstBody, "client_metadata.x-codex-turn-metadata").String(), "session_id").String(); got != mappedSession {
		t.Fatalf("embedded session = %q, want %q", got, mappedSession)
	}

	headers := http.Header{
		"Session-Id":              []string{"client-session-a"},
		"Thread-Id":               []string{"client-thread-a"},
		"X-Codex-Installation-Id": []string{"client-install-a"},
	}
	applyCodexAccountIdentityHeaders(headers, &first)
	if got := headers.Get("Session-Id"); got != mappedSession {
		t.Fatalf("mapped Session-Id = %q, want body session %q", got, mappedSession)
	}
	if got, want := headers.Get("Thread-Id"), gjson.GetBytes(firstBody, "client_metadata.thread_id").String(); got != want {
		t.Fatalf("mapped Thread-Id = %q, want body thread %q", got, want)
	}

	differentCallerBody, _ := applyCodexAccountIdentityBody(codexAccountIdentityTestContext("caller-key-b"), auth, body)
	if got := gjson.GetBytes(differentCallerBody, "client_metadata.session_id").String(); got == mappedSession {
		t.Fatal("different downstream API keys must not share an upstream session identity")
	}
	differentAuth := &cliproxyauth.Auth{
		ID:       "local-auth-b",
		Provider: "codex",
		Metadata: map[string]any{"account_id": "upstream-account-b"},
	}
	differentAccountBody, _ := applyCodexAccountIdentityBody(codexAccountIdentityTestContext("caller-key-a"), differentAuth, body)
	if got := gjson.GetBytes(differentAccountBody, "client_metadata.session_id").String(); got == mappedSession {
		t.Fatal("different OAuth accounts must not share an upstream session identity")
	}
	differentSession := []byte(`{"prompt_cache_key":"client-session-b","client_metadata":{"session_id":"client-session-b","thread_id":"client-thread-b"}}`)
	differentSessionBody, _ := applyCodexAccountIdentityBody(codexAccountIdentityTestContext("caller-key-a"), auth, differentSession)
	if got := gjson.GetBytes(differentSessionBody, "client_metadata.session_id").String(); got == mappedSession {
		t.Fatal("different client sessions must remain distinct after remapping")
	}
}

func TestCodexAccountIdentityUsesStableOAuthAccountAndComposesWithDeviceMode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := codexAccountIdentityTestContext("caller-key")
	body := []byte(`{"prompt_cache_key":"client-session","client_metadata":{"session_id":"client-session","thread_id":"client-thread","x-codex-installation-id":"client-install"}}`)
	firstAuth := &cliproxyauth.Auth{ID: "row-a", Provider: "codex", Metadata: map[string]any{"account_id": "shared-upstream-account"}}
	secondAuth := &cliproxyauth.Auth{ID: "row-b", Provider: "codex", Metadata: map[string]any{"account_id": "shared-upstream-account"}}

	firstScoped, firstIdentity := applyCodexAccountIdentityBody(ctx, firstAuth, body)
	secondScoped, _ := applyCodexAccountIdentityBody(ctx, secondAuth, body)
	if string(firstScoped) != string(secondScoped) {
		t.Fatal("two local auth rows for the same OAuth account must share an identity namespace")
	}

	cfg := &config.Config{Codex: config.CodexConfig{IdentityConvergence: true}}
	firstFinal, firstConvergence := applyCodexIdentityConvergenceBody(cfg, firstAuth, firstScoped, firstScoped, nil)
	secondFinal, secondConvergence := applyCodexIdentityConvergenceBody(cfg, secondAuth, secondScoped, secondScoped, nil)
	if firstConvergence.mode != codexIdentityConvergenceDevice || secondConvergence.mode != codexIdentityConvergenceDevice {
		t.Fatal("global convergence must compose with account scoping in device mode")
	}
	if firstConvergence.installationID != secondConvergence.installationID {
		t.Fatal("the same OAuth account must retain one converged device across local auth rows")
	}
	if got, want := gjson.GetBytes(firstFinal, "client_metadata.session_id").String(), gjson.GetBytes(firstScoped, "client_metadata.session_id").String(); got != want {
		t.Fatalf("device mode changed scoped session from %q to %q", want, got)
	}
	if got, want := gjson.GetBytes(secondFinal, "prompt_cache_key").String(), gjson.GetBytes(secondScoped, "prompt_cache_key").String(); got != want {
		t.Fatalf("device mode changed scoped prompt cache key from %q to %q", want, got)
	}

	headers := http.Header{"Session-Id": []string{"client-session"}, "X-Codex-Installation-Id": []string{"client-install"}}
	applyCodexAccountIdentityHeaders(headers, &firstIdentity)
	applyCodexIdentityConvergenceHeaders(headers, &firstConvergence)
	if got, want := headers.Get("Session-Id"), gjson.GetBytes(firstFinal, "client_metadata.session_id").String(); got != want {
		t.Fatalf("composed Session-Id = %q, want %q", got, want)
	}
	if got := headers.Get("X-Codex-Installation-Id"); got != firstConvergence.installationID {
		t.Fatalf("composed installation header = %q, want %q", got, firstConvergence.installationID)
	}
}

func TestCodexAccountIdentityLeavesAPIKeyAndMissingFieldsUntouched(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"gpt-5.6"}`)
	oauth := &cliproxyauth.Auth{ID: "oauth", Provider: "codex", Metadata: map[string]any{"account_id": "account"}}
	oauthBody, state := applyCodexAccountIdentityBody(codexAccountIdentityTestContext("caller"), oauth, body)
	if !state.enabled || string(oauthBody) != string(body) {
		t.Fatal("OAuth account scoping must not synthesize missing identity fields")
	}
	apiKeyAuth := &cliproxyauth.Auth{ID: "api-key", Provider: "codex", Attributes: map[string]string{"api_key": "upstream-key"}}
	apiKeyBody, apiKeyState := applyCodexAccountIdentityBody(codexAccountIdentityTestContext("caller"), apiKeyAuth, body)
	if apiKeyState.enabled || string(apiKeyBody) != string(body) {
		t.Fatal("Codex API-key upstreams must remain untouched")
	}
	childBody := []byte(`{"client_metadata":{"x-codex-parent-thread-id":"parent","x-openai-subagent":"review"}}`)
	untouched, apiKeyState := applyCodexAccountIdentityBody(codexAccountIdentityTestContext("caller"), apiKeyAuth, childBody)
	headers := http.Header{"X-Codex-Parent-Thread-Id": {"parent"}, "X-Openai-Subagent": {"review"}}
	applyCodexAccountIdentityHeaders(headers, &apiKeyState)
	if string(untouched) != string(childBody) || headers.Get("X-Codex-Parent-Thread-Id") != "parent" || headers.Get("X-OpenAI-Subagent") != "review" {
		t.Fatal("API-key child identities must remain untouched")
	}
}

func TestCodexChildIdentityOnWire(t *testing.T) {
	for _, transport := range []string{"http", "ws"} {
		for _, stream := range []bool{false, true} {
			for _, source := range []string{"both", "header_only", "body_only", "absent"} {
				t.Run(fmt.Sprintf("%s/stream%v/%s", transport, stream, source), func(t *testing.T) {
					type requestCapture struct {
						headers http.Header
						body    []byte
					}
					captured := make(chan requestCapture, 1)
					completed := []byte(`{"type":"response.completed","response":{"id":"resp_child","status":"completed","output":[]}}`)
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if transport == "ws" {
							upgrader := websocket.Upgrader{}
							conn, err := upgrader.Upgrade(w, r, nil)
							if err != nil {
								t.Error(err)
								return
							}
							defer func() { _ = conn.Close() }()
							_, body, err := conn.ReadMessage()
							if err != nil {
								t.Error(err)
								return
							}
							captured <- requestCapture{r.Header.Clone(), body}
							if err = conn.WriteMessage(websocket.TextMessage, completed); err != nil {
								t.Error(err)
							}
							return
						}
						body, err := io.ReadAll(r.Body)
						if err != nil {
							t.Error(err)
							return
						}
						captured <- requestCapture{r.Header.Clone(), body}
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = fmt.Fprintf(w, "data: %s\n\n", completed)
					}))
					defer server.Close()
					cfg := &config.Config{
						Codex:               config.CodexConfig{IdentityConvergence: true},
						CodexHeaderDefaults: config.CodexHeaderDefaults{UserAgent: "codex-tui/0.162.0 (Mac OS 27.0.1; arm64)"},
					}
					var executor cliproxyauth.ProviderExecutor = NewCodexExecutor(cfg)
					if transport == "ws" {
						executor = NewCodexWebsocketsExecutor(cfg)
					}
					auth := &cliproxyauth.Auth{ID: "child-wire", Provider: "codex", Attributes: map[string]string{"base_url": server.URL}, Metadata: map[string]any{"account_id": "synthetic-account", "access_token": "synthetic-token"}}
					ctx := codexAccountIdentityTestContext("synthetic-caller")
					parentBody, _ := applyCodexAccountIdentityBody(ctx, auth, []byte(`{"client_metadata":{"thread_id":"parent-thread"}}`))
					parent := gjson.GetBytes(parentBody, "client_metadata.thread_id").String()
					metadata := map[string]string{"session_id": "parent-thread", "thread_id": "child-thread"}
					hasBody := source == "both" || source == "body_only"
					hasHeaders := source == "both" || source == "header_only"
					if hasBody {
						metadata["x-codex-parent-thread-id"] = "parent-thread"
						metadata["x-openai-subagent"] = "review"
						metadata["x-codex-turn-metadata"] = `{"parent_thread_id":"parent-thread","x-codex-parent-thread-id":"parent-thread","thread_id":"child-thread"}`
					}
					body, err := json.Marshal(map[string]any{"model": "gpt-5.5", "input": []any{}, "prompt_cache_key": "parent-thread", "client_metadata": metadata})
					if err != nil {
						t.Fatal(err)
					}
					headers := http.Header{"User-Agent": {"Go-http-client/1.1"}, "Session-Id": {"parent-thread"}, "Thread-Id": {"child-thread"}}
					if hasHeaders {
						headers.Set("X-Codex-Parent-Thread-Id", "parent-thread")
						headers.Set("X-OpenAI-Subagent", "review")
					}
					req := cliproxyexecutor.Request{Model: "gpt-5.5", Payload: body}
					opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatCodex, Headers: headers, Stream: stream}
					if stream {
						result, err := executor.ExecuteStream(ctx, auth, req, opts)
						if err != nil {
							t.Fatal(err)
						}
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								t.Fatal(chunk.Err)
							}
						}
					} else if _, err := executor.Execute(ctx, auth, req, opts); err != nil {
						t.Fatal(err)
					}
					got := <-captured
					if got.headers.Get("User-Agent") != cfg.CodexHeaderDefaults.UserAgent {
						t.Fatal("configured UA must replace the intermediary's Go UA")
					}
					child := got.headers.Get("Thread-Id")
					if parent == "parent-thread" || child == "" || child == "child-thread" || child == parent || got.headers.Get("Session-Id") != parent {
						t.Fatal("child and parent thread relationship lost")
					}
					if hasHeaders {
						if got.headers.Get("X-Codex-Parent-Thread-Id") != parent || got.headers.Get("X-OpenAI-Subagent") != "review" {
							t.Fatal("independent parent or subagent header dropped or incorrectly mapped")
						}
						if headers.Get("X-Codex-Parent-Thread-Id") != "parent-thread" {
							t.Fatal("incoming headers mutated")
						}
					} else if got.headers.Get("X-Codex-Parent-Thread-Id") != "" || got.headers.Get("X-OpenAI-Subagent") != "" {
						t.Fatal("missing child headers must not be synthesized")
					}
					alias := gjson.GetBytes(got.body, "client_metadata.x-codex-parent-thread-id")
					if hasBody {
						nested := gjson.Parse(gjson.GetBytes(got.body, "client_metadata.x-codex-turn-metadata").String())
						if alias.String() != parent || nested.Get("parent_thread_id").String() != parent || nested.Get("x-codex-parent-thread-id").String() != parent || nested.Get("thread_id").String() != child {
							t.Fatal("body aliases and nested parent metadata must match the parent thread")
						}
						if gjson.GetBytes(got.body, "client_metadata.x-openai-subagent").String() != "review" {
							t.Fatal("subagent label changed")
						}
					} else if alias.Exists() {
						t.Fatal("missing body parent must not be synthesized")
					}
				})
			}
		}
	}
}
