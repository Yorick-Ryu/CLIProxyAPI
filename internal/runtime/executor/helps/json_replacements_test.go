package helps

import (
	"bytes"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestReplaceJSONValuesPreservesSourceAndUntouchedBytes(t *testing.T) {
	original := `  {"first":"old", "nested": { "value" : 2 }, "last":false}  `
	body := []byte(original)
	first := util.GetGJSONBytesNoCopy(body, "first")
	nested := util.GetGJSONBytesNoCopy(body, "nested.value")
	out, ok := ReplaceJSONValues(body, []JSONValueReplacement{{nested, []byte(`1234`)}, {first, []byte(`"new"`)}})
	if !ok || string(out) != `  {"first":"new", "nested": { "value" : 1234 }, "last":false}  ` {
		t.Fatalf("unexpected result: %s (ok=%v)", out, ok)
	}
	if string(body) != original || first.String() != "old" {
		t.Fatal("source buffer or its read-only view changed")
	}
}

func TestReplaceJSONValuesRejectsInvalidAndOverlappingRanges(t *testing.T) {
	body := []byte(`{"a":{"b":1}}`)
	for _, edits := range [][]JSONValueReplacement{
		{{Value: gjson.Result{Index: -1, Raw: "x"}, Raw: []byte(`0`)}},
		{{Value: gjson.Result{Index: len(body), Raw: "x"}, Raw: []byte(`0`)}},
		{{Value: gjson.Result{Index: 1, Raw: "wrong"}, Raw: []byte(`0`)}},
		{{Value: util.GetGJSONBytesNoCopy(body, "a"), Raw: []byte(`{}`)}, {Value: util.GetGJSONBytesNoCopy(body, "a.b"), Raw: []byte(`2`)}},
	} {
		out, ok := ReplaceJSONValues(body, edits)
		if ok || !bytes.Equal(out, body) {
			t.Fatal("invalid replacement was accepted")
		}
	}
}

func TestDeleteTopLevelJSONFieldsMatchesSequentialDeletes(t *testing.T) {
	for _, input := range []string{
		`{"input":[{"keep":true}]}`,
		`{"drop":null,"keep":1,"remove":false}`,
		` { "keep": 1, "drop": [1,2], "remove": {} } `,
		`{"drop":1,"drop":2,"keep":{"drop":3}}`,
		`{"dr\u006fp":1,"remove":2}`,
		`{}`,
	} {
		body := []byte(input)
		want, _ := sjson.DeleteBytes(body, "drop")
		want, _ = sjson.DeleteBytes(want, "remove")
		got := DeleteTopLevelJSONFields(body, "drop", "remove")
		if !bytes.Equal(got, want) || string(body) != input {
			t.Fatalf("delete differs for %s: got %s want %s", input, got, want)
		}
	}
	body := []byte(`{"keep":1}`)
	got := DeleteTopLevelJSONFields(body, "missing", "also_missing")
	if &got[0] != &body[0] {
		t.Fatal("missing fields should not copy the request")
	}
}
