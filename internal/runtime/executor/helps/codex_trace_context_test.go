package helps

import (
	"net/http"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
)

func TestStripCodexTraceContextBody(t *testing.T) {
	body := []byte(` {"traceparent":"root","tracestate":"root-state","input":[{"traceparent":"business"}],"tools":[{"parameters":{"tracestate":"schema"}}],"client_metadata":{"ws_request_header_traceparent":"a","ws_request_header_traceparent":"duplicate","ws_request_header_tracestate":null,"thread_id":"thread","unknown":{"traceparent":"extension"},"turn_metadata":{"traceparent":"nested","keep":9007199254740993},"x-codex-turn-metadata":"{\"tracestate\":\"nested\",\"keep\":1e+09}"},"turnMetadata":{"ws_request_header_traceparent":"top-meta","keep":true}} `)
	got := StripCodexTraceContextBody(body)
	for _, path := range []string{"traceparent", "tracestate", "client_metadata.ws_request_header_traceparent", "client_metadata.ws_request_header_tracestate", "client_metadata.turn_metadata.traceparent", "turnMetadata.ws_request_header_traceparent"} {
		if ojson.GetBytes(got, path).Exists() {
			t.Errorf("trace remains at %s", path)
		}
	}
	nested := ojson.GetBytes(got, "client_metadata.x-codex-turn-metadata").String()
	if ojson.Get(nested, "tracestate").Exists() || !strings.Contains(nested, "1e+09") {
		t.Fatal("embedded trace or numeric representation changed incorrectly")
	}
	for _, path := range []string{"input", "tools", "client_metadata.thread_id", "client_metadata.unknown", "client_metadata.turn_metadata.keep"} {
		if ojson.GetBytes(body, path).Raw != ojson.GetBytes(got, path).Raw {
			t.Errorf("business value changed: %s", path)
		}
	}
	if string(StripCodexTraceContextBody(got)) != string(got) {
		t.Fatal("cleanup is not idempotent")
	}
	for _, raw := range []string{`{"input":[],"client_metadata":{"keep":1e+09}}`, `{"client_metadata":"opaque"}`, `{"client_metadata":{"turn_metadata":"not JSON"}}`, `{"client_metadata":null}`} {
		if string(StripCodexTraceContextBody([]byte(raw))) != raw {
			t.Errorf("unrelated body changed: %s", raw)
		}
	}
}
func TestStripCodexTraceContextHeaders(t *testing.T) {
	h := http.Header{"traceparent": {"a"}, "TraceParent": {"b"}, "TRACESTATE": {"c"}, "ws_request_header_traceparent": {"d"}, "X-Codex-Turn-Metadata": {`{"traceparent":"x","thread_id":"keep"}`, `{"tracestate":"y"}`}, "X-Business": {"unchanged"}}
	StripCodexTraceContextHeaders(h)
	if len(h) != 2 || h.Get("X-Business") != "unchanged" {
		t.Fatal("unexpected header cleanup")
	}
	if len(h.Values("X-Codex-Turn-Metadata")) != 2 || ojson.Get(h.Get("X-Codex-Turn-Metadata"), "thread_id").String() != "keep" {
		t.Fatal("metadata values lost")
	}
	for _, raw := range h.Values("X-Codex-Turn-Metadata") {
		if ojson.Get(raw, "traceparent").Exists() || ojson.Get(raw, "tracestate").Exists() {
			t.Fatal("embedded trace retained")
		}
	}
}
