package helps

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// The golden file was generated with the unmodified native ASCII formatter and
// struct definitions from openai/codex 3fd08b540570a5dd9c61d98560a0231f7b43891f.
// Reference-only owned strings replace borrowed strings and string-valued IDs;
// serde field order, skip rules, BTreeMaps and tagged tool sources are retained.
func TestCanonicalCodexTurnMetadataNativeFixture(t *testing.T) {
	input, err := os.ReadFile("testdata/codex_turn_metadata_native_input.json")
	if err != nil {
		t.Fatal(err)
	}
	golden, err := os.ReadFile("testdata/codex_turn_metadata_native_golden.json")
	if err != nil {
		t.Fatal(err)
	}
	want := string(bytes.TrimSpace(golden))
	if got := CanonicalCodexTurnMetadata(string(input)); got != want {
		t.Fatalf("metadata differs from native Rust serialization\ngot:  %q\nwant: %q", got, want)
	}
	if got := CanonicalCodexTurnMetadata(want); got != want {
		t.Fatal("native metadata must round-trip byte-identically")
	}
	for _, character := range want {
		if character > 0x7f {
			t.Fatal("native metadata must be ASCII-safe")
		}
	}
}

func TestCanonicalCodexTurnMetadataExtensionsAndInvalidInput(t *testing.T) {
	for _, test := range []struct{ name, input, want string }{
		{"extensions", `{"z":[{"session_id":null,"agent_name":"extension","n":1.2300}],"analytics_enabled":false,"window_number":0,"a":1e+03}`, `{"window_number":0,"analytics_enabled":false,"a":1e+03,"z":[{"agent_name":"extension","n":1.2300,"session_id":null}]}`},
		{"integer precision", `{"unknown":-9223372036854775808,"turn_started_at_unix_ms":9223372036854775807,"window_number":18446744073709551615}`, `{"window_number":18446744073709551615,"turn_started_at_unix_ms":9223372036854775807,"unknown":-9223372036854775808}`},
		{"explicit null", `{"analytics_enabled":null,"agent_name":null}`, `{"agent_name":null,"analytics_enabled":null}`},
		{"empty object", ` { } `, `{}`},
		{"malformed", `{"turn_id":`, `{"turn_id":`},
		{"trailing value", `{"turn_id":"x"} {}`, `{"turn_id":"x"} {}`},
		{"array", `[{"turn_id":"x"}]`, `[{"turn_id":"x"}]`},
		{"null", `null`, `null`},
		{"duplicate", `{"turn_id":"a","turn_id":"b"}`, `{"turn_id":"a","turn_id":"b"}`},
		{"nested duplicate", `{"extension":{"x":1,"x":2}}`, `{"extension":{"x":1,"x":2}}`},
		{"unpaired surrogate", `{"agent_name":"\ud800"}`, `{"agent_name":"\ud800"}`},
		{"unpaired key surrogate", `{"\udfff":"keep"}`, `{"\udfff":"keep"}`},
		{"invalid UTF8", "{\"agent_name\":\"\xff\"}", "{\"agent_name\":\"\xff\"}"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := CanonicalCodexTurnMetadata(test.input); got != test.want {
				t.Fatalf("got %q, want %q", got, test.want)
			}
		})
	}
}

func TestCanonicalCodexTurnMetadataBodyAndHeaders(t *testing.T) {
	const original = `{"z":"東京🚀<>&/\b\f\u007f","history_initialization":"new","turn_id":"turn","agent_name":"agent"}`
	want := `{"agent_name":"agent","turn_id":"turn","history_initialization":"new","z":"\u6771\u4eac\ud83d\ude80<>&/\b\f` + "\x7f" + `"}`
	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	for _, asObject := range []bool{false, true} {
		value := string(encoded)
		if asObject {
			value = original
		}
		body := []byte(` {"unrelated":1.2300,"client_metadata":{"x-codex-turn-metadata":` + value + `,"keep":"東京"},"tail":true} `)
		before := bytes.Clone(body)
		got := CanonicalCodexTurnMetadataBody(body)
		metadata := gjson.GetBytes(got, "client_metadata.x-codex-turn-metadata")
		formatted := metadata.Str
		if asObject {
			formatted = metadata.Raw
		}
		if metadata.IsObject() != asObject || formatted != want {
			t.Fatalf("metadata type or serialization changed: %q", got)
		}
		if !bytes.Equal(body, before) || gjson.GetBytes(got, "unrelated").Raw != "1.2300" || gjson.GetBytes(got, "client_metadata.keep").String() != "東京" || !bytes.HasPrefix(got, []byte(` {"unrelated":1.2300,`)) || !bytes.HasSuffix(got, []byte(`,"tail":true} `)) {
			t.Fatal("formatting changed caller-owned or unrelated request bytes")
		}
	}
	for _, body := range []string{`{}`, `{"client_metadata":{}}`, `{"client_metadata":{"x-codex-turn-metadata":null}}`, `{"client_metadata":{"x-codex-turn-metadata":[]}}`, `{"client_metadata":{"x-codex-turn-metadata":"not-json"}}`, `{"client_metadata":{"x-codex-turn-metadata":"{\"agent_name\":\"\ud800\"}"}}`} {
		if got := CanonicalCodexTurnMetadataBody([]byte(body)); string(got) != body {
			t.Fatalf("absent or invalid metadata changed: %s", got)
		}
	}
	headerOriginal := strings.ReplaceAll(original, `\u007f`, "")
	headerWant := strings.ReplaceAll(want, "\x7f", "")
	values := []string{headerOriginal, headerOriginal}
	headers := http.Header{"x-CoDeX-turn-metadata": values, "X-Unrelated": {"keep"}}
	CanonicalCodexTurnMetadataHeaders(headers)
	if !reflect.DeepEqual(headers["x-CoDeX-turn-metadata"], []string{headerWant, headerWant}) || headers.Get("X-Unrelated") != "keep" || values[0] != headerOriginal {
		t.Fatal("formatting changed header casing, multiplicity or shared values")
	}
	withDEL := http.Header{"X-Codex-Turn-Metadata": {original}}
	CanonicalCodexTurnMetadataHeaders(withDEL)
	if withDEL.Get("X-Codex-Turn-Metadata") != original {
		t.Fatal("canonicalization introduced an invalid DEL byte into a header")
	}
	empty := http.Header{}
	CanonicalCodexTurnMetadataHeaders(empty)
	CanonicalCodexTurnMetadataHeaders(nil)
	if len(empty) != 0 {
		t.Fatal("formatting synthesized a missing header")
	}
}

func TestCodexTurnMetadataHeaderOmitsOnlyToolInventory(t *testing.T) {
	const raw = ` {"extension":18446744073709551615,"tool_namespaces_info":{"functions":{"name":"functions","functions":{}}},"thread_id":"thread"} `
	const want = `{"thread_id":"thread","extension":18446744073709551615}`
	headers := http.Header{"x-CoDeX-TuRn-MeTaDaTa": {raw, `{"tool_namespaces_info":{},"tool_namespaces_info":{},"extension":"ambiguous"}`}}
	original := append([]string(nil), headers["x-CoDeX-TuRn-MeTaDaTa"]...)
	CanonicalCodexTurnMetadataHeaders(headers)
	if headers["x-CoDeX-TuRn-MeTaDaTa"][0] != want || headers["x-CoDeX-TuRn-MeTaDaTa"][1] != original[1] {
		t.Fatal("header projection lost an unknown field, precision, or invalid input")
	}
	if original[0] != raw {
		t.Fatal("shared header values were mutated")
	}
	body := []byte(`{"client_metadata":{"x-codex-turn-metadata":{"tool_namespaces_info":{"n":{"name":"n","functions":{}}},"extension":"keep"}}}`)
	normalized := CanonicalCodexTurnMetadataBody(body)
	if !gjson.GetBytes(normalized, "client_metadata.x-codex-turn-metadata.tool_namespaces_info").Exists() {
		t.Fatal("header projection removed inventory from the body")
	}
}
