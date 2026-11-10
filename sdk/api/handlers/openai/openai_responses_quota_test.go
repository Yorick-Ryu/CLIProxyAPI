package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

const quotaMetadata = `{"type":"codex.response.metadata","headers":{"x-codex-primary-used-percent":"80","X-Codex-Credits-Balance":"20","x-models-etag":"keep-etag","x-codex-turn-state":"turn-1","x-codex-safety-buffering-enabled":"true"},"future":{"value":1}}`
const quotaDelta = `{"type":"response.output_text.delta","delta":"x-codex-primary-used-percent must remain in ordinary text"}`
const quotaCompleted = `{"type":"response.completed","response":{"id":"resp-quota","status":"completed","output":[],"usage":{"input_tokens":7,"output_tokens":3}}}`

func assertQuotaMetadata(t *testing.T, payload []byte, stripped bool) {
	t.Helper()
	var got, want map[string]any
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(quotaMetadata), &want); err != nil {
		t.Fatal(err)
	}
	if stripped {
		delete(want["headers"].(map[string]any), "x-codex-primary-used-percent")
		delete(want["headers"].(map[string]any), "X-Codex-Credits-Balance")
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("metadata changed beyond the selected header: got %s, want %#v", payload, want)
	}
}

func TestFilterResponsesQuota(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		input := []byte(quotaMetadata)
		got := filterResponsesQuota(input, "codex.response.metadata", enabled)
		assertQuotaMetadata(t, got, enabled)
		if string(input) != quotaMetadata {
			t.Fatal("filter mutated the upstream payload")
		}
		if !enabled && &got[0] != &input[0] {
			t.Fatal("disabled filter copied the payload")
		}
	}
	for _, tc := range []struct{ name, payload, event string }{
		{"delta", quotaDelta, "response.output_text.delta"},
		{"completed", quotaCompleted, "response.completed"},
		{"missing headers", `{"type":"codex.response.metadata","future":1}`, "codex.response.metadata"},
		{"null headers", `{"type":"codex.response.metadata","headers":null}`, "codex.response.metadata"},
		{"array headers", `{"type":"codex.response.metadata","headers":[]}`, "codex.response.metadata"},
		{"other header", `{"headers":{"x-codex-turn-state":"t"}}`, "codex.response.metadata"},
		{"invalid", `{"headers":{"x-codex-primary-used-percent":"v"},`, "codex.response.metadata"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := []byte(tc.payload)
			got := filterResponsesQuota(input, tc.event, true)
			if &got[0] != &input[0] || !bytes.Equal(got, input) {
				t.Fatalf("unexpected payload rewrite: %s", got)
			}
		})
	}
	for _, key := range []string{`x-codex-primary-used-percent`, `X-CODEX-CREDITS-BALANCE`, `x-codex-credits-\u0062alance`, `x-codex-gpt5.3-primary-reset-at`} {
		input := []byte(`{"type":"codex.response.metadata","headers":{"` + key + `":"v"}}`)
		got := filterResponsesQuota(input, "codex.response.metadata", true)
		if ojson.GetBytes(got, "headers").Raw != "{}" {
			t.Fatalf("header %s survived: %s", key, got)
		}
	}
}

func TestResponsesQuotaSSEFragmentation(t *testing.T) {
	prefix := "id: keep-id\r\nevent: codex.response.metadata\r\ndata: "
	frame := []byte(prefix + quotaMetadata + "\r\n\r\n")
	// Split at every JSON byte, without splitting SSE field names themselves.
	for split := len(prefix); split < len(prefix)+len(quotaMetadata); split++ {
		var out bytes.Buffer
		f := &responsesSSEFramer{isCodexClient: true, stripQuota: true}
		f.WriteChunk(&out, frame[:split])
		f.WriteChunk(&out, frame[split:])
		f.Flush(&out)
		payload, ok := responsesSSEDataPayload(out.Bytes())
		if !ok {
			t.Fatalf("missing metadata at split %d: %s", split, out.Bytes())
		}
		assertQuotaMetadata(t, payload, true)
		if !bytes.Contains(out.Bytes(), []byte("id: keep-id")) {
			t.Fatalf("lost SSE ID at split %d", split)
		}
	}
}

type quotaExecutor struct{ websocketCaptureExecutor }

func (*quotaExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{Payload: []byte(`{"id":"resp-quota","output":[]}`), Headers: quotaHeaders()}, nil
}

func quotaHeaders() http.Header {
	return http.Header{"X-Codex-Primary-Used-Percent": {"80"}, "X-Codex-Credits-Balance": {"20"}, "X-Models-Etag": {"keep-etag"}, "X-Codex-Turn-State": {"turn-1"}, "ETag": {`"keep-own-etag"`}}
}

func (*quotaExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	chunks := make(chan coreexecutor.StreamChunk, 5)
	for _, payload := range []string{`{"type":"response.created","response":{"id":"resp-quota"}}`, quotaMetadata, `{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":80}},"credits":{"balance":"20"}}`, quotaDelta, quotaCompleted} {
		chunks <- coreexecutor.StreamChunk{Payload: []byte("data: " + payload + "\n\n")}
	}
	close(chunks)
	return &coreexecutor.StreamResult{Headers: quotaHeaders(), Chunks: chunks}, nil
}

func TestResponsesQuotaTransportsAndReload(t *testing.T) {
	gin.SetMode(gin.TestMode)
	executor := &quotaExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	auth := &coreauth.Auth{ID: "quota-auth", Provider: executor.Identifier(), Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "quota-test"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	base := handlers.NewBaseAPIHandlers(&config.SDKConfig{PassthroughHeaders: true}, manager)
	h := NewOpenAIResponsesAPIHandler(base)
	router := gin.New()
	router.POST("/v1/responses", h.Responses)
	router.POST("/v1/responses/compact", h.Compact)
	router.GET("/v1/responses", h.ResponsesWebsocket)
	server := httptest.NewServer(router)
	defer server.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", http.Header{"User-Agent": {"codex_cli_rs/0.167.0"}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// Reuse one WebSocket while config changes, including switching filtering off.
	for _, enabled := range []bool{false, true, false} {
		cfg := &config.SDKConfig{PassthroughHeaders: true}
		cfg.Client.Codex.StripQuota = enabled
		base.UpdateClients(cfg)
		for _, tc := range []struct {
			path   string
			stream bool
		}{{"/v1/responses", false}, {"/v1/responses", true}, {"/v1/responses/compact", false}} {
			body := fmt.Sprintf(`{"model":"quota-test","input":[],"stream":%t}`, tc.stream)
			req, err := http.NewRequest(http.MethodPost, server.URL+tc.path, strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("User-Agent", "codex_cli_rs/0.167.0")
			resp, err := server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status %d: %s", resp.StatusCode, data)
			}
			if (resp.Header.Get("X-Codex-Primary-Used-Percent") == "") != enabled {
				t.Fatalf("unexpected models header: %v", resp.Header)
			}
			if (resp.Header.Get("X-Codex-Credits-Balance") == "") != enabled || resp.Header.Get("X-Models-Etag") != "keep-etag" {
				t.Fatal("quota or etag header changed unexpectedly")
			}
			if resp.Header.Get("X-Codex-Turn-State") != "turn-1" || resp.Header.Get("ETag") != `"keep-own-etag"` {
				t.Fatalf("other headers changed: %v", resp.Header)
			}
			if tc.stream {
				seen := false
				for _, payload := range websocketJSONPayloadsFromChunk(data) {
					if ojson.GetBytes(payload, "type").String() == "codex.response.metadata" {
						assertQuotaMetadata(t, payload, enabled)
						seen = true
					}
				}
				if !seen || !bytes.Contains(data, []byte(quotaDelta)) || !bytes.Contains(data, []byte(`"input_tokens":7`)) {
					t.Fatalf("lost events or usage: %s", data)
				}
			}
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"quota-test","input":[]}`)); err != nil {
			t.Fatal(err)
		}
		if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Fatal(err)
		}
		seenMetadata, seenDelta, seenQuota := false, false, false
		for {
			_, payload, err := conn.ReadMessage()
			if err != nil {
				t.Fatal(err)
			}
			switch ojson.GetBytes(payload, "type").String() {
			case "codex.response.metadata":
				assertQuotaMetadata(t, payload, enabled)
				seenMetadata = true
			case "response.output_text.delta":
				if string(payload) != quotaDelta {
					t.Fatalf("delta rewritten: %s", payload)
				}
				seenDelta = true
			case "codex.rate_limits":
				seenQuota = true
			case "response.completed":
				if seenQuota == enabled {
					t.Fatalf("quota event presence=%t enabled=%t", seenQuota, enabled)
				}
				if !seenMetadata || !seenDelta || ojson.GetBytes(payload, "response.usage.input_tokens").Int() != 7 {
					t.Fatalf("lost metadata, delta or usage: %s", payload)
				}
				goto completed
			case "error":
				t.Fatalf("unexpected error: %s", payload)
			}
		}
	completed:
	}
}

var quotaBenchSink []byte

func BenchmarkResponsesQuota(b *testing.B) {
	for _, tc := range []struct{ name, payload, event string }{
		{"delta", quotaDelta, "response.output_text.delta"},
		{"large_delta", `{"type":"response.output_text.delta","delta":"` + strings.Repeat("x", 65536) + `"}`, "response.output_text.delta"},
		{"metadata", `{"type":"codex.response.metadata","headers":{"x-codex-primary-used-percent":"W/\"upstream\"","x-codex-turn-state":"turn-1"}}`, "codex.response.metadata"},
		{"metadata_case_variants", quotaMetadata, "codex.response.metadata"},
		{"metadata_without_quota", `{"type":"codex.response.metadata","headers":{"x-codex-turn-state":"t"}}`, "codex.response.metadata"},
	} {
		for _, enabled := range []bool{false, true} {
			b.Run(fmt.Sprintf("%s/enabled=%t", tc.name, enabled), func(b *testing.B) {
				payload := []byte(tc.payload)
				b.ReportAllocs()
				for b.Loop() {
					quotaBenchSink = filterResponsesQuota(payload, tc.event, enabled)
				}
			})
		}
	}
}

func TestResponsesQuotaNestedHeadersAndIndependentFlags(t *testing.T) {
	for _, event := range []string{"response.metadata", "response.created", "response.completed", "response.failed", "error"} {
		input := []byte(`{"type":"` + event + `","headers":{"X-Codex-Primary-Used-Percent":"80","Retry-After":"30"},"response":{"headers":{"x-codex-credits-balance":"20","openai-model":"keep"},"usage":{"input_tokens":7}},"error":{"code":"rate_limit_exceeded","headers":{"x-codex-secondary-reset-at":"123"}}}`)
		got := filterResponsesQuota(input, event, true)
		if bytes.Contains(got, []byte("x-codex-")) || bytes.Contains(got, []byte("X-Codex-")) {
			t.Fatalf("quota leaked: %s", got)
		}
		if ojson.GetBytes(got, "headers.Retry-After").String() != "30" || ojson.GetBytes(got, "response.usage.input_tokens").Int() != 7 || ojson.GetBytes(got, "error.code").String() != "rate_limit_exceeded" {
			t.Fatalf("lost unrelated data: %s", got)
		}
	}
	for _, etag := range []bool{false, true} {
		for _, quota := range []bool{false, true} {
			var out bytes.Buffer
			f := &responsesSSEFramer{isCodexClient: true, stripModelsETag: etag, stripQuota: quota}
			f.WriteChunk(&out, []byte("data: "+quotaMetadata+"\n\n"))
			f.Flush(&out)
			payload, _ := responsesSSEDataPayload(out.Bytes())
			if ojson.GetBytes(payload, "headers.x-models-etag").Exists() == etag || ojson.GetBytes(payload, "headers.x-codex-primary-used-percent").Exists() == quota {
				t.Fatalf("flags interfere: etag=%t quota=%t payload=%s", etag, quota, payload)
			}
		}
	}
}

func TestResponsesQuotaSSEEventNameOnly(t *testing.T) {
	var out bytes.Buffer
	f := &responsesSSEFramer{isCodexClient: true, stripQuota: true}
	f.WriteChunk(&out, []byte("event: codex.response.metadata\ndata: {\"headers\":{\"x-codex-credits-balance\":\"20\",\"x-codex-turn-state\":\"keep\"}}\n\n"))
	f.Flush(&out)
	payload, _ := responsesSSEDataPayload(out.Bytes())
	if ojson.GetBytes(payload, "headers.x-codex-credits-balance").Exists() || ojson.GetBytes(payload, "headers.x-codex-turn-state").String() != "keep" {
		t.Fatalf("unexpected metadata: %s", out.Bytes())
	}
}
