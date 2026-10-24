package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/klauspost/compress/zstd"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

type batchWireConn struct {
	net.Conn
	mu       sync.Mutex
	received bytes.Buffer
}

func (c *batchWireConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.mu.Lock()
	_, _ = c.received.Write(p[:n])
	c.mu.Unlock()
	return n, err
}
func (c *batchWireConn) snapshot() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return bytes.Clone(c.received.Bytes())
}

type batchWireListener struct{ net.Listener }

func (l batchWireListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &batchWireConn{Conn: c}, nil
}

type batchWireContextKey struct{}
type batchWireResult struct{ raw, payload, encoded []byte }

// This opt-in harness runs unchanged in independent before/after source trees.
// It captures bytes read from TCP before net/http and Gorilla decode them.
func TestCodexBodyBatchWireCapture(t *testing.T) {
	output := os.Getenv("CPA_BODY_CAPTURE_DIR")
	address := os.Getenv("CPA_BODY_CAPTURE_ADDR")
	if output == "" || address == "" {
		t.Skip("set capture directory and fixed loopback address for paired capture")
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil || host != "127.0.0.1" {
		t.Fatal("capture receiver must bind IPv4 loopback")
	}
	const turn = "0199c123-4567-7890-8123-456789abcdef"
	metadata := `{"turn_id":"` + turn + `","session_id":"session","thread_id":"thread","root_turn_id":"root","parent_turn_id":"parent","parent_thread_id":"parent-thread","request_kind":"foreground","window_number":3,"analytics_enabled":true,"tool_namespaces_info":{"z":{"keep":true}},"unknown":{"z":1e+09,"a":"中文"}}`
	var input []any
	for i := 0; i < 32; i++ {
		input = append(input, map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": fmt.Sprintf("history %d 中文 %s", i, strings.Repeat("x", 128))}}, "internal_chat_message_metadata_passthrough": map[string]any{"turn_id": fmt.Sprintf("history-turn-%d", i)}})
	}
	source, err := json.Marshal(map[string]any{"model": "gpt-6-astra", "instructions": "fixture instructions", "prompt_cache_key": "session", "client_metadata": map[string]any{"session_id": "session", "thread_id": "thread", "turn_id": turn, "window_number": 3, "x-codex-window-id": "thread:3", "x-codex-turn-metadata": metadata}, "input": input, "unknown": "keep"})
	if err != nil {
		t.Fatal(err)
	}
	for _, transport := range []string{"http", "ws"} {
		for _, stream := range []bool{false, true} {
			for _, compression := range []bool{false, true} {
				for _, mode := range []string{"off", "device", "session", "full", "api-key"} {
					for _, rules := range []string{"none", "override", "filter"} {
						name := fmt.Sprintf("%s_stream%t_compress%t_%s_%s", transport, stream, compression, mode, rules)
						t.Run(name, func(t *testing.T) {
							got := make(chan batchWireResult, 1)
							complete := []byte(`{"type":"response.completed","response":{"id":"capture-response","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":0,"total_tokens":1}}}`)
							server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
								recorded := r.Context().Value(batchWireContextKey{}).(*batchWireConn)
								if transport == "ws" {
									peer, err := (&websocket.Upgrader{EnableCompression: compression}).Upgrade(w, r, nil)
									if err != nil {
										t.Error(err)
										return
									}
									defer func() { _ = peer.Close() }()
									_, payload, err := peer.ReadMessage()
									if err != nil {
										t.Error(err)
										return
									}
									got <- batchWireResult{raw: recorded.snapshot(), payload: payload}
									if err := peer.WriteMessage(websocket.TextMessage, complete); err != nil {
										t.Error(err)
									}
									return
								}
								encoded, err := io.ReadAll(r.Body)
								if err != nil {
									t.Error(err)
									return
								}
								payload := encoded
								if r.Header.Get("Content-Encoding") == "zstd" {
									decoder, err := zstd.NewReader(nil)
									if err != nil {
										t.Error(err)
										return
									}
									defer decoder.Close()
									payload, err = decoder.DecodeAll(encoded, nil)
									if err != nil {
										t.Error(err)
										return
									}
								}
								got <- batchWireResult{raw: recorded.snapshot(), payload: payload, encoded: encoded}
								w.Header().Set("Content-Type", "text/event-stream")
								_, _ = w.Write(append(append([]byte("data: "), complete...), '\n', '\n'))
							}))
							_ = server.Listener.Close()
							listener, err := net.Listen("tcp", address)
							if err != nil {
								t.Fatal(err)
							}
							server.Listener = batchWireListener{listener}
							server.Config.ConnContext = func(ctx context.Context, c net.Conn) context.Context {
								return context.WithValue(ctx, batchWireContextKey{}, c)
							}
							server.Start()
							defer server.Close()
							cfg := &config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}, Codex: config.CodexConfig{HTTPTLSProfile: "linux", RequestCompression: config.CodexRequestCompressionConfig{Enabled: compression}}}
							if rules == "override" {
								cfg.Payload.Override = []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "*"}}, Params: map[string]any{"client_metadata.x-codex-turn-metadata": ` {"analytics_enabled":false,"marker":"explicit"} `, "input.0.internal_chat_message_metadata_passthrough.turn_id": "explicit-turn"}}}
							}
							if rules == "filter" {
								cfg.Payload.Filter = []config.PayloadFilterRule{{Models: []config.PayloadModelRule{{Name: "*"}}, Params: []string{"client_metadata", "prompt_cache_key", "input.1"}}}
							}
							credential := &cliproxyauth.Auth{ID: "capture-" + name, Provider: "codex", Attributes: map[string]string{"base_url": server.URL, "header:X-Operator-Fixture": "unchanged"}, Metadata: map[string]any{"account_id": "synthetic-account", "access_token": "synthetic-fixture", codexIdentityConvergenceModeKey: mode}}
							if mode == "api-key" {
								credential.Attributes["api_key"] = "synthetic-fixture"
							}
							request := cliproxyexecutor.Request{Model: "gpt-6-astra", Payload: bytes.Clone(source)}
							opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatCodex, Headers: http.Header{"Session-Id": {"session"}, "Thread-Id": {"thread"}, "X-Client-Request-Id": {"thread"}, "X-Codex-Parent-Thread-Id": {"parent-thread"}, "X-Codex-Window-Id": {"thread:3"}, "X-Codex-Turn-Metadata": {metadata}, "Version": {"0.163.0"}, "User-Agent": {"capture-client/1"}}}
							var executor cliproxyauth.ProviderExecutor = NewCodexExecutor(cfg)
							if transport == "ws" {
								executor = NewCodexWebsocketsExecutor(cfg)
							}
							if stream {
								result, err := executor.ExecuteStream(context.Background(), credential, request, opts)
								if err != nil {
									t.Fatal(err)
								}
								for chunk := range result.Chunks {
									if chunk.Err != nil {
										t.Fatal(chunk.Err)
									}
								}
							} else if _, err := executor.Execute(context.Background(), credential, request, opts); err != nil {
								t.Fatal(err)
							}
							capture := <-got
							split := bytes.Index(capture.raw, []byte("\r\n\r\n"))
							if split < 0 {
								t.Fatal("missing raw HTTP header boundary")
							}
							dir := filepath.Join(output, name)
							if err := os.MkdirAll(dir, 0700); err != nil {
								t.Fatal(err)
							}
							for suffix, data := range map[string][]byte{"tcp": capture.raw, "headers": capture.raw[:split+4], "payload": capture.payload, "encoded": capture.encoded} {
								if err := os.WriteFile(filepath.Join(dir, "request."+suffix), data, 0600); err != nil {
									t.Fatal(err)
								}
							}
							if !bytes.Equal(request.Payload, source) {
								t.Fatal("executor mutated source payload")
							}
						})
					}
				}
			}
		}
	}
}
