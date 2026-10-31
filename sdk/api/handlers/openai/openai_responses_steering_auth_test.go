package openai

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

func TestResponsesSteeringDisabledAccountCannotSendAnotherFrame(t *testing.T) {
	var frames atomic.Int32
	done := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = c.Close() }()
		_ = c.SetReadDeadline(time.Now().Add(8 * time.Second))
		if _, _, err := c.ReadMessage(); err != nil {
			t.Error(err)
			return
		}
		frames.Add(1)
		_ = c.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"r1"}}`))
		_ = c.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"r1","output":[]}}`))
		if _, _, err := c.ReadMessage(); err == nil {
			frames.Add(1)
		}
	}))
	defer upstream.Close()
	cfg := &config.Config{}
	cfg.Codex.ResponseSteering = true
	cfg.CodexResponseSteering = true
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfig(cfg)
	manager.RegisterExecutor(runtimeexecutor.NewCodexAutoExecutor(cfg))
	id := "steering-disable-test"
	auth, err := manager.Register(context.Background(), &coreauth.Auth{ID: id, Provider: "codex", Status: coreauth.StatusActive, Attributes: map[string]string{"api_key": "test-key", "base_url": upstream.URL, "websockets": "true"}})
	if err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(id, "codex", []*registry.ModelInfo{{ID: "steering-disable-model"}})
	defer registry.GetGlobalRegistry().UnregisterClient(id)
	h := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&cfg.SDKConfig, manager))
	router := gin.New()
	router.GET("/v1/responses", h.ResponsesWebsocket)
	downstream := httptest.NewServer(router)
	defer downstream.Close()
	c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(downstream.URL, "http")+"/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetReadDeadline(time.Now().Add(8 * time.Second))
	if err := c.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"steering-disable-model","input":[]}`)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, _, err := c.ReadMessage(); err != nil {
			t.Fatal(err)
		}
	}
	disabled := auth.Clone()
	disabled.Disabled = true
	disabled.Status = coreauth.StatusDisabled
	if _, err := manager.Update(context.Background(), disabled); err != nil {
		t.Fatal(err)
	}
	if err := c.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.steer","previous_response_id":"r1","input":"must not be sent"}`)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.ReadMessage(); err == nil {
		t.Fatal("disabled account connection remained usable")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream close stalled")
	}
	if frames.Load() != 1 {
		t.Fatalf("disabled credential sent %d frames", frames.Load())
	}
}

type environmentContextRewriteInterceptorHost struct {
	handlers.PluginInterceptorHost
	afterSawRemovedHeader atomic.Bool
	afterMissingKept      atomic.Bool
}

func (*environmentContextRewriteInterceptorHost) HasStreamInterceptors() bool { return false }

func (*environmentContextRewriteInterceptorHost) HasRequestInterceptors() bool { return true }

func (*environmentContextRewriteInterceptorHost) InterceptRequestBeforeAuth(_ context.Context, req pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
	var headers http.Header
	if req.Headers != nil {
		headers = req.Headers.Clone()
		headers.Del("X-Strip-In-Before")
	}
	if !bytes.Contains(req.Body, []byte("<timezone>UTC</timezone>")) {
		return pluginapi.RequestInterceptResponse{Headers: headers}
	}
	patched := bytes.ReplaceAll(req.Body, []byte("<timezone>UTC</timezone>"), []byte("<timezone>Asia/Shanghai</timezone>"))
	return pluginapi.RequestInterceptResponse{Headers: headers, Body: patched}
}

func (h *environmentContextRewriteInterceptorHost) InterceptRequestAfterAuth(_ context.Context, req pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
	if req.Headers.Get("X-Strip-In-Before") != "" {
		h.afterSawRemovedHeader.Store(true)
	}
	if req.Headers.Get("X-Keep-Header") != "kept" {
		h.afterMissingKept.Store(true)
	}
	if !bytes.Contains(req.Body, []byte("<current_date>2026-04-19</current_date>")) {
		return pluginapi.RequestInterceptResponse{
			Headers:      http.Header{"X-Codex-Beta-Features": []string{"siwc"}},
			ClearHeaders: []string{"X-Codex-Beta-Features"},
		}
	}
	patched := bytes.ReplaceAll(req.Body, []byte("<current_date>2026-04-19</current_date>"), []byte("<current_date>2026-04-20</current_date>"))
	return pluginapi.RequestInterceptResponse{
		Headers:      http.Header{"X-Codex-Beta-Features": []string{"siwc"}},
		ClearHeaders: []string{"X-Codex-Beta-Features"},
		Body:         patched,
	}
}

func TestResponsesSteeringSubsequentWebsocketMessagesRunRequestInterceptors(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const (
		authID    = "codex-steering-env-rewrite-auth"
		modelName = "steering-env-rewrite-model"
	)
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: modelName}})
	t.Cleanup(func() {
		reg.UnregisterClient(authID)
	})

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstreamFrames := make(chan []byte, 8)
	upstreamWS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		for {
			_, msg, errRead := conn.ReadMessage()
			if errRead != nil {
				return
			}
			upstreamFrames <- bytes.Clone(msg)
			switch gjson.GetBytes(msg, "type").String() {
			case "response.create", "response.append":
				if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"resp_1"}}`)); err != nil {
					return
				}
				if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_1"}}`)); err != nil {
					return
				}
			case "response.steer":
				if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.output_text.delta","delta":"steered"}`)); err != nil {
					return
				}
			}
		}
	}))
	defer upstreamWS.Close()

	cfg := &config.Config{}
	cfg.Codex.ResponseSteering = true
	cfg.CodexResponseSteering = true

	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfig(cfg)
	manager.RegisterExecutor(runtimeexecutor.NewCodexAutoExecutor(cfg))
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "codex-steering-env-rewrite-auth",
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"base_url":   upstreamWS.URL,
			"api_key":    "upstream-token",
			"websockets": "true",
		},
	}); err != nil {
		t.Fatalf("Register auth failed: %v", err)
	}

	interceptorHost := &environmentContextRewriteInterceptorHost{}
	baseHandler := handlers.NewBaseAPIHandlers(&cfg.SDKConfig, manager)
	baseHandler.SetPluginHost(interceptorHost)
	responsesHandler := NewOpenAIResponsesAPIHandler(baseHandler)

	router := gin.New()
	router.GET("/v1/responses", responsesHandler.ResponsesWebsocket)
	proxyServer := httptest.NewServer(router)
	defer proxyServer.Close()

	wsURL := "ws" + strings.TrimPrefix(proxyServer.URL, "http") + "/v1/responses"
	dialHeaders := http.Header{
		"X-Strip-In-Before": []string{"should-be-removed"},
		"X-Keep-Header":     []string{"kept"},
	}
	c, resp, err := websocket.DefaultDialer.Dial(wsURL, dialHeaders)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("Dial failed: %v", err)
	}
	defer func() { _ = c.Close() }()

	assertRewrittenUpstreamFrame := func(step string) {
		t.Helper()
		select {
		case frame := <-upstreamFrames:
			if bytes.Contains(frame, []byte("<timezone>UTC</timezone>")) {
				t.Fatalf("%s upstream frame still contains unpatched timezone: %s", step, frame)
			}
			if !bytes.Contains(frame, []byte("<timezone>Asia/Shanghai</timezone>")) {
				t.Fatalf("%s upstream frame missing before-auth patched timezone: %s", step, frame)
			}
			if bytes.Contains(frame, []byte("<current_date>2026-04-19</current_date>")) {
				t.Fatalf("%s upstream frame still contains unpatched current_date: %s", step, frame)
			}
			if !bytes.Contains(frame, []byte("<current_date>2026-04-20</current_date>")) {
				t.Fatalf("%s upstream frame missing after-auth patched current_date: %s", step, frame)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("%s timed out waiting for upstream frame", step)
		}
	}

	// 1. Initial response.create
	turn1 := `{"type":"response.create","model":"` + modelName + `","input":[{"role":"developer","content":[{"type":"input_text","text":"<environment_context>\n  <current_date>2026-04-19</current_date>\n  <timezone>UTC</timezone>\n</environment_context>"}]}]}`
	if err := c.WriteMessage(websocket.TextMessage, []byte(turn1)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, _, errRead := c.ReadMessage(); errRead != nil {
			t.Fatalf("initial turn read failed: %v", errRead)
		}
	}
	assertRewrittenUpstreamFrame("initial response.create")

	// 2. Subsequent response.create over the same duplex WebSocket
	turn2 := `{"type":"response.create","model":"` + modelName + `","input":[{"role":"developer","content":[{"type":"input_text","text":"<environment_context>\n  <current_date>2026-04-19</current_date>\n  <timezone>UTC</timezone>\n</environment_context>"}]}]}`
	if err := c.WriteMessage(websocket.TextMessage, []byte(turn2)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, _, errRead := c.ReadMessage(); errRead != nil {
			t.Fatalf("subsequent response.create read failed: %v", errRead)
		}
	}
	assertRewrittenUpstreamFrame("subsequent response.create")

	// 3. Subsequent response.append over the same duplex WebSocket
	turn3 := `{"type":"response.append","input":[{"role":"developer","content":[{"type":"input_text","text":"<environment_context>\n  <current_date>2026-04-19</current_date>\n  <timezone>UTC</timezone>\n</environment_context>"}]}]}`
	if err := c.WriteMessage(websocket.TextMessage, []byte(turn3)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, _, errRead := c.ReadMessage(); errRead != nil {
			t.Fatalf("subsequent response.append read failed: %v", errRead)
		}
	}
	assertRewrittenUpstreamFrame("subsequent response.append")

	// 4. Subsequent response.steer over the same duplex WebSocket
	turn4 := `{"type":"response.steer","input":[{"role":"user","content":[{"type":"input_text","text":"<environment_context>\n  <current_date>2026-04-19</current_date>\n  <timezone>UTC</timezone>\n</environment_context>"}]}]}`
	if err := c.WriteMessage(websocket.TextMessage, []byte(turn4)); err != nil {
		t.Fatal(err)
	}
	if _, _, errRead := c.ReadMessage(); errRead != nil {
		t.Fatalf("subsequent response.steer read failed: %v", errRead)
	}
	assertRewrittenUpstreamFrame("subsequent response.steer")
	if interceptorHost.afterSawRemovedHeader.Load() {
		t.Fatal("after-auth interceptor saw header X-Strip-In-Before that was removed by before-auth interceptor")
	}
	if interceptorHost.afterMissingKept.Load() {
		t.Fatal("after-auth interceptor lost X-Keep-Header across turns")
	}
}
