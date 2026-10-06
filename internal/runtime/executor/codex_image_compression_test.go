package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

func TestCodexDirectImageCompressionAfterPayloadConfig(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, path := range []string{codexImagesGenerationsPath, codexImagesEditsPath} {
			for _, test := range []struct {
				name           string
				initialSize    int
				finalSize      int
				disabled       bool
				apiKey         bool
				multipart      bool
				wantCompressed bool
			}{
				{name: "small", initialSize: 100, finalSize: 100},
				{name: "large", initialSize: 100000, finalSize: 100000, wantCompressed: true},
				{name: "grow", initialSize: 100, finalSize: 100000, wantCompressed: true},
				{name: "shrink", initialSize: 100000, finalSize: 100},
				{name: "disabled", initialSize: 100000, finalSize: 100000, disabled: true},
				{name: "api_key", initialSize: 100000, finalSize: 100000, apiKey: true},
				{name: "multipart", initialSize: 100000, finalSize: 100000, multipart: true, wantCompressed: true},
			} {
				if test.multipart && path != codexImagesEditsPath {
					continue
				}
				t.Run(fmt.Sprintf("%s/stream=%t/%s", path, stream, test.name), func(t *testing.T) {
					server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
						wireBody, errRead := io.ReadAll(request.Body)
						if errRead != nil {
							t.Error(errRead)
							writer.WriteHeader(http.StatusBadRequest)
							return
						}
						if request.URL.Path != strings.TrimPrefix(path, "/v1") {
							t.Errorf("unexpected path: %s", request.URL.Path)
						}
						if request.ContentLength != int64(len(wireBody)) {
							t.Errorf("ContentLength=%d, wire bytes=%d", request.ContentLength, len(wireBody))
						}
						body := wireBody
						if test.wantCompressed {
							if request.Header.Get("Content-Encoding") != "zstd" {
								t.Error("missing zstd header")
							}
							decoder, errDecoder := zstd.NewReader(nil)
							if errDecoder != nil {
								t.Error(errDecoder)
								return
							}
							defer decoder.Close()
							body, errRead = decoder.DecodeAll(wireBody, nil)
							if errRead != nil {
								t.Errorf("invalid compressed request: %v", errRead)
								writer.WriteHeader(http.StatusBadRequest)
								return
							}
						} else if request.Header.Get("Content-Encoding") != "" {
							t.Error("unexpected compression header")
						}
						if !json.Valid(body) || gjson.GetBytes(body, "padding").String() != strings.Repeat("x", test.finalSize) {
							t.Errorf("wire body does not contain final configured payload: type=%s prefix=%.180s", request.Header.Get("Content-Type"), body)
						}
						if gjson.GetBytes(body, "quality").String() != "high" || gjson.GetBytes(body, "markers").Raw != `["keep"]` {
							t.Error("payload rules must run exactly once before compression")
						}
						if stream {
							writer.Header().Set("Content-Type", "text/event-stream")
							_, _ = io.WriteString(writer, "data: {\"type\":\"image_generation.completed\",\"b64_json\":\"AA==\"}\n\n")
						} else {
							writer.Header().Set("Content-Type", "application/json")
							_, _ = io.WriteString(writer, `{"created":1,"data":[{"b64_json":"AA=="}]}`)
						}
					}))
					defer server.Close()
					cfg := &config.Config{
						Codex: config.CodexConfig{RequestCompression: config.CodexRequestCompressionConfig{Enabled: !test.disabled, MinBytes: 65536}},
						Payload: config.PayloadConfig{
							Override: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "gpt-image-2"}}, Params: map[string]any{"padding": strings.Repeat("x", test.finalSize), "quality": "high"}}},
							Filter:   []config.PayloadFilterRule{{Models: []config.PayloadModelRule{{Name: "gpt-image-2"}}, Params: []string{"markers.0"}}},
						},
					}
					auth := &cliproxyauth.Auth{ID: "image-compression", Provider: "codex", Attributes: map[string]string{"base_url": server.URL}}
					if test.apiKey {
						auth.Attributes["api_key"] = "test-key"
					}
					payload, errMarshal := json.Marshal(map[string]any{"model": "gpt-image-2", "prompt": "test", "quality": "auto", "padding": strings.Repeat("x", test.initialSize), "markers": []string{"remove", "keep"}, "images": []map[string]string{{"file_id": "test-image"}}})
					if errMarshal != nil {
						t.Fatal(errMarshal)
					}
					opts := codexOpenAIImageTestOptions(path, stream)
					if test.multipart {
						var buffer bytes.Buffer
						writer := multipart.NewWriter(&buffer)
						for key, value := range map[string]string{"model": "gpt-image-2", "prompt": "test", "padding": strings.Repeat("x", test.initialSize)} {
							if errWrite := writer.WriteField(key, value); errWrite != nil {
								t.Fatal(errWrite)
							}
						}
						for _, value := range []string{"remove", "keep"} {
							if errWrite := writer.WriteField("markers", value); errWrite != nil {
								t.Fatal(errWrite)
							}
						}
						part, errPart := writer.CreateFormFile("image", "test.png")
						if errPart != nil {
							t.Fatal(errPart)
						}
						if _, errWrite := part.Write([]byte("test-image")); errWrite != nil {
							t.Fatal(errWrite)
						}
						if errClose := writer.Close(); errClose != nil {
							t.Fatal(errClose)
						}
						payload = buffer.Bytes()
						opts.Headers = http.Header{"Content-Type": []string{writer.FormDataContentType()}}
					}
					executor := NewCodexExecutor(cfg)
					req := cliproxyexecutor.Request{Model: "gpt-image-2", Payload: payload}
					if stream {
						result, errExecute := executor.ExecuteStream(context.Background(), auth, req, opts)
						if errExecute != nil {
							t.Fatal(errExecute)
						}
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								t.Fatal(chunk.Err)
							}
						}
					} else if _, errExecute := executor.Execute(context.Background(), auth, req, opts); errExecute != nil {
						t.Fatal(errExecute)
					}
				})
			}
		}
	}
}

func TestCodexRequestBodyCompressionReplay(t *testing.T) {
	executor := NewCodexExecutor(&config.Config{Codex: config.CodexConfig{RequestCompression: config.CodexRequestCompressionConfig{Enabled: true, MinBytes: 65536}}})
	request, errRequest := http.NewRequest(http.MethodPost, "http://localhost/images/edits", nil)
	if errRequest != nil {
		t.Fatal(errRequest)
	}
	for _, size := range []int{65535, 65536, 65537, 0} {
		body := bytes.Repeat([]byte("x"), size)
		if errSet := executor.setCodexRequestBody(request, &cliproxyauth.Auth{}, body); errSet != nil {
			t.Fatal(errSet)
		}
		wire, errRead := io.ReadAll(request.Body)
		if errRead != nil {
			t.Fatal(errRead)
		}
		replay, errReplay := request.GetBody()
		if errReplay != nil {
			t.Fatal(errReplay)
		}
		replayed, errRead := io.ReadAll(replay)
		if errClose := replay.Close(); errClose != nil {
			t.Fatal(errClose)
		}
		if errRead != nil || !bytes.Equal(wire, replayed) || request.ContentLength != int64(len(wire)) {
			t.Fatal("request length or replay differs from wire body")
		}
		if size >= 65536 {
			decoder, errDecoder := zstd.NewReader(nil)
			if errDecoder != nil {
				t.Fatal(errDecoder)
			}
			decoded, errDecode := decoder.DecodeAll(wire, nil)
			decoder.Close()
			if errDecode != nil || !bytes.Equal(decoded, body) || request.Header.Get("Content-Encoding") != "zstd" {
				t.Fatal("compressed body or encoding is invalid")
			}
		} else if request.Header.Get("Content-Encoding") != "" || !bytes.Equal(wire, body) {
			t.Fatal("uncompressed body retains stale compression")
		}
	}
}
