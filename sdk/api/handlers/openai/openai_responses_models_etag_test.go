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

const modelsETagMetadata = `{"type":"codex.response.metadata","headers":{"x-models-etag":"W/\"upstream\"","X-Models-Etag":"another","x-codex-turn-state":"turn-1","x-codex-safety-buffering-enabled":"true"},"future":{"value":1}}`
const modelsETagDelta = `{"type":"response.output_text.delta","delta":"x-models-etag must remain in ordinary text"}`
const modelsETagCompleted = `{"type":"response.completed","response":{"id":"resp-etag","status":"completed","output":[],"usage":{"input_tokens":7,"output_tokens":3}}}`

func assertModelsETagMetadata(t *testing.T, payload []byte, stripped bool) {
	t.Helper()
	var got, want map[string]any
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(modelsETagMetadata), &want); err != nil {
		t.Fatal(err)
	}
	if stripped {
		delete(want["headers"].(map[string]any), "x-models-etag")
		delete(want["headers"].(map[string]any), "X-Models-Etag")
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("metadata changed beyond the selected header: got %s, want %#v", payload, want)
	}
}

func TestFilterResponsesModelsETag(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		input := []byte(modelsETagMetadata)
		got := filterResponsesModelsETag(input, "codex.response.metadata", enabled)
		assertModelsETagMetadata(t, got, enabled)
		if string(input) != modelsETagMetadata {
			t.Fatal("filter mutated the upstream payload")
		}
		if !enabled && &got[0] != &input[0] {
			t.Fatal("disabled filter copied the payload")
		}
	}
	for _, tc := range []struct{ name, payload, event string }{
		{"delta", modelsETagDelta, "response.output_text.delta"},
		{"completed", modelsETagCompleted, "response.completed"},
		{"missing headers", `{"type":"codex.response.metadata","future":1}`, "codex.response.metadata"},
		{"null headers", `{"type":"codex.response.metadata","headers":null}`, "codex.response.metadata"},
		{"array headers", `{"type":"codex.response.metadata","headers":[]}`, "codex.response.metadata"},
		{"other header", `{"headers":{"x-codex-turn-state":"t"}}`, "codex.response.metadata"},
		{"invalid", `{"headers":{"x-models-etag":"v"},`, "codex.response.metadata"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := []byte(tc.payload)
			got := filterResponsesModelsETag(input, tc.event, true)
			if &got[0] != &input[0] || !bytes.Equal(got, input) {
				t.Fatalf("unexpected payload rewrite: %s", got)
			}
		})
	}
	for _, key := range []string{`x-models-etag`, `X-MODELS-ETAG`, `x-models-\u0065tag`} {
		input := []byte(`{"type":"codex.response.metadata","headers":{"` + key + `":"v"}}`)
		got := filterResponsesModelsETag(input, "codex.response.metadata", true)
		if ojson.GetBytes(got, "headers").Raw != "{}" {
			t.Fatalf("header %s survived: %s", key, got)
		}
	}
}

func TestResponsesModelsETagSSEFragmentation(t *testing.T) {
	prefix := "id: keep-id\r\nevent: codex.response.metadata\r\ndata: "
	frame := []byte(prefix + modelsETagMetadata + "\r\n\r\n")
	// Split at every JSON byte, without splitting SSE field names themselves.
	for split := len(prefix); split < len(prefix)+len(modelsETagMetadata); split++ {
		var out bytes.Buffer
		f := &responsesSSEFramer{isCodexClient: true, stripModelsETag: true}
		f.WriteChunk(&out, frame[:split])
		f.WriteChunk(&out, frame[split:])
		f.Flush(&out)
		payload, ok := responsesSSEDataPayload(out.Bytes())
		if !ok {
			t.Fatalf("missing metadata at split %d: %s", split, out.Bytes())
		}
		assertModelsETagMetadata(t, payload, true)
		if !bytes.Contains(out.Bytes(), []byte("id: keep-id")) {
			t.Fatalf("lost SSE ID at split %d", split)
		}
	}
}

type modelsETagExecutor struct{ websocketCaptureExecutor }

func (*modelsETagExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{Payload: []byte(`{"id":"resp-etag","output":[]}`), Headers: modelsETagHeaders()}, nil
}

func modelsETagHeaders() http.Header {
	return http.Header{"X-Models-Etag": {`W/"upstream"`}, "X-Codex-Turn-State": {"turn-1"}, "ETag": {`"keep-own-etag"`}}
}

func (*modelsETagExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	chunks := make(chan coreexecutor.StreamChunk, 4)
	for _, payload := range []string{`{"type":"response.created","response":{"id":"resp-etag"}}`, modelsETagMetadata, modelsETagDelta, modelsETagCompleted} {
		chunks <- coreexecutor.StreamChunk{Payload: []byte("data: " + payload + "\n\n")}
	}
	close(chunks)
	return &coreexecutor.StreamResult{Headers: modelsETagHeaders(), Chunks: chunks}, nil
}

func TestResponsesModelsETagTransportsAndReload(t *testing.T) {
	gin.SetMode(gin.TestMode)
	executor := &modelsETagExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	auth := &coreauth.Auth{ID: "models-etag-auth", Provider: executor.Identifier(), Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "models-etag-test"}})
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
		cfg.Client.Codex.StripModelsETag = enabled
		base.UpdateClients(cfg)
		for _, tc := range []struct {
			path   string
			stream bool
		}{{"/v1/responses", false}, {"/v1/responses", true}, {"/v1/responses/compact", false}} {
			body := fmt.Sprintf(`{"model":"models-etag-test","input":[],"stream":%t}`, tc.stream)
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
			if (resp.Header.Get("X-Models-Etag") == "") != enabled {
				t.Fatalf("unexpected models header: %v", resp.Header)
			}
			if resp.Header.Get("X-Codex-Turn-State") != "turn-1" || resp.Header.Get("ETag") != `"keep-own-etag"` {
				t.Fatalf("other headers changed: %v", resp.Header)
			}
			if tc.stream {
				seen := false
				for _, payload := range websocketJSONPayloadsFromChunk(data) {
					if ojson.GetBytes(payload, "type").String() == "codex.response.metadata" {
						assertModelsETagMetadata(t, payload, enabled)
						seen = true
					}
				}
				if !seen || !bytes.Contains(data, []byte(modelsETagDelta)) || !bytes.Contains(data, []byte(`"input_tokens":7`)) {
					t.Fatalf("lost events or usage: %s", data)
				}
			}
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"models-etag-test","input":[]}`)); err != nil {
			t.Fatal(err)
		}
		if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Fatal(err)
		}
		seenMetadata, seenDelta := false, false
		for {
			_, payload, err := conn.ReadMessage()
			if err != nil {
				t.Fatal(err)
			}
			switch ojson.GetBytes(payload, "type").String() {
			case "codex.response.metadata":
				assertModelsETagMetadata(t, payload, enabled)
				seenMetadata = true
			case "response.output_text.delta":
				if string(payload) != modelsETagDelta {
					t.Fatalf("delta rewritten: %s", payload)
				}
				seenDelta = true
			case "response.completed":
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

var modelsETagBenchSink []byte

func BenchmarkResponsesModelsETag(b *testing.B) {
	for _, tc := range []struct{ name, payload, event string }{
		{"delta", modelsETagDelta, "response.output_text.delta"},
		{"large_delta", `{"type":"response.output_text.delta","delta":"` + strings.Repeat("x", 65536) + `"}`, "response.output_text.delta"},
		{"metadata", `{"type":"codex.response.metadata","headers":{"x-models-etag":"W/\"upstream\"","x-codex-turn-state":"turn-1"}}`, "codex.response.metadata"},
		{"metadata_case_variants", modelsETagMetadata, "codex.response.metadata"},
		{"metadata_without_etag", `{"type":"codex.response.metadata","headers":{"x-codex-turn-state":"t"}}`, "codex.response.metadata"},
	} {
		for _, enabled := range []bool{false, true} {
			b.Run(fmt.Sprintf("%s/enabled=%t", tc.name, enabled), func(b *testing.B) {
				payload := []byte(tc.payload)
				b.ReportAllocs()
				for b.Loop() {
					modelsETagBenchSink = filterResponsesModelsETag(payload, tc.event, enabled)
				}
			})
		}
	}
}
