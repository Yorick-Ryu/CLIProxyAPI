package helps

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"strings"
	"testing"
)

// Snapshot of the sequential cleanup used before combining metadata edits.
func sequentialStripCodexTraceObject(raw string) string {
	if !gjson.Parse(raw).IsObject() || !gjson.Valid(raw) {
		return raw
	}
	for _, key := range codexTraceFields {
		for gjson.Get(raw, key).Exists() {
			updated, err := sjson.Delete(raw, key)
			if err != nil || updated == raw {
				break
			}
			raw = updated
		}
	}
	return raw
}

func sequentialStripCodexTraceMetadata(body []byte, field string) []byte {
	value := util.GetGJSONBytesNoCopy(body, field)
	switch {
	case value.Type == gjson.String:
		if updated := sequentialStripCodexTraceObject(value.Str); updated != value.Str {
			result, err := sjson.SetBytes(body, field, updated)
			if err == nil {
				return result
			}
		}
	case value.IsObject():
		if updated := sequentialStripCodexTraceObject(value.Raw); updated != value.Raw {
			result, ok := ReplaceJSONValues(body, []JSONValueReplacement{{Value: value, Raw: []byte(updated)}})
			if ok {
				return result
			}
		}
	}
	return body
}

// sequentialStripCodexTraceContextBody breaks downstream distributed-trace correlation at
// the OAuth account boundary. Call before explicit operator payload rules.
// Only protocol metadata locations are visited; input and tools are untouched.
func sequentialStripCodexTraceContextBody(body []byte) []byte {
	// Avoid validating or copying a large request when no trace field is present.
	for _, key := range codexTraceFields {
		for util.GetGJSONBytesNoCopy(body, key).Exists() {
			updated, err := sjson.DeleteBytes(body, key)
			if err != nil || len(updated) == len(body) {
				break
			}
			body = updated
		}
	}
	for _, key := range codexTraceMetadataFields {
		body = sequentialStripCodexTraceMetadata(body, key)
	}
	metadata := util.GetGJSONBytesNoCopy(body, "client_metadata")
	if !metadata.IsObject() {
		return body
	}
	updated := []byte(sequentialStripCodexTraceObject(metadata.Raw))
	for _, key := range codexTraceMetadataFields {
		updated = sequentialStripCodexTraceMetadata(updated, key)
	}
	if string(updated) == metadata.Raw {
		return body
	}
	result, ok := ReplaceJSONValues(body, []JSONValueReplacement{{Value: metadata, Raw: updated}})
	if ok {
		return result
	}
	return body
}

func traceBatchFixture(size int, trace bool) []byte {
	metadata := `{"x-codex-turn-metadata":"{\"z\":\"<>&東京\",\"agent_name\":\"test\"}"}`
	if trace {
		metadata = `{"ws_request_header_traceparent":"client-trace","ws_request_header_tracestate":"client-state","x-codex-turn-metadata":"{\"z\":\"<>&東京\",\"traceparent\":\"nested\",\"agent_name\":\"test\"}"}`
	}
	return []byte(`{"model":"test","input":[{"text":"` + strings.Repeat("x", size) + `"}],"client_metadata":` + metadata + `}`)
}
func TestPrepareCodexRequestMetadataMatchesSequential(t *testing.T) {
	fixtures := []string{
		string(traceBatchFixture(1024, true)), string(traceBatchFixture(1024, false)),
		` { "traceparent":"a", "tracestate":"b", "keep":1e+09 } `,
		`{"keep":9007199254740993,"traceparent":"a","tracestate":"b"}`,
		`{"traceparent":"a","tracestate":"b"}`,
		`{"traceparent":"a","traceparent":"b","keep":true,"tracestate":"c"}`,
		`{"turnMetadata":{"traceparent":"a","keep":true},"client_metadata":{"traceparent":"b","turn_metadata":"{\"tracestate\":\"c\"}","x-codex-turn-metadata":{"z":"東京","agent_name":"test","tracestate":"d"}},"ws_request_header_traceparent":"last"}`,
		`{"client_metadata":{"x-codex-turn-metadata":"not JSON","unknown":{"traceparent":"business"}},"input":[{"traceparent":"business"}]}`,
		`{"client_metadata":{"x-codex-turn-metadata":"{\"z\":\"\\ud800\",\"traceparent\":\"a\"}"}}`,
	}
	for i, raw := range fixtures {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			body := []byte(raw)
			want := CanonicalCodexTurnMetadataBody(sequentialStripCodexTraceContextBody(body))
			got := PrepareCodexRequestMetadata(body)
			// Removing fields may leave different insignificant whitespace at object boundaries.
			if !gjson.ValidBytes(got) {
				t.Fatalf("invalid result: %s", got)
			}
			if string(got) != string(want) {
				var a, b bytes.Buffer
				if err := json.Compact(&a, got); err != nil {
					t.Fatal(err)
				}
				if err := json.Compact(&b, want); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(a.Bytes(), b.Bytes()) {
					t.Fatalf("got %s\nwant %s", got, want)
				}
			}
			if string(body) != raw {
				t.Fatal("input was mutated")
			}
		})
	}
}
func TestPrepareCodexRequestMetadataNoChangeReusesBody(t *testing.T) {
	body := []byte(`{"input":[{"traceparent":"business"}],"client_metadata":{"unknown":true}}`)
	got := PrepareCodexRequestMetadata(body)
	if &got[0] != &body[0] {
		t.Fatal("unchanged request was copied")
	}
}
func BenchmarkCodexTraceMetadataBatch(b *testing.B) {
	for _, size := range []int{1024, 1024 * 1024} {
		for _, trace := range []bool{false, true} {
			body := traceBatchFixture(size, trace)
			for _, combined := range []bool{false, true} {
				b.Run(fmt.Sprintf("bytes%d/trace%v/combined%v", size, trace, combined), func(b *testing.B) {
					b.ReportAllocs()
					b.SetBytes(int64(len(body)))
					for b.Loop() {
						if combined {
							PrepareCodexRequestMetadata(body)
						} else {
							CanonicalCodexTurnMetadataBody(sequentialStripCodexTraceContextBody(body))
						}
					}
				})
			}
		}
	}
}
