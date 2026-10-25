package helps

import (
	"sort"

	"github.com/tidwall/gjson"
)

// JSONValueReplacement replaces an existing value identified in the unchanged
// source document. Raw must contain an encoded JSON value.
type JSONValueReplacement struct {
	Value gjson.Result
	Raw   []byte
}

// ReplaceJSONValues writes disjoint replacements into one new buffer. It never
// changes payload, so read-only GJSON views of the source remain valid.
func ReplaceJSONValues(payload []byte, edits []JSONValueReplacement) ([]byte, bool) {
	if len(edits) == 0 {
		return payload, true
	}
	sort.Slice(edits, func(i, j int) bool { return edits[i].Value.Index < edits[j].Value.Index })
	size, end := len(payload), 0
	for _, edit := range edits {
		start := edit.Value.Index
		if start < end || start < 0 || start > len(payload) || len(edit.Value.Raw) == 0 || len(edit.Value.Raw) > len(payload)-start {
			return payload, false
		}
		end = start + len(edit.Value.Raw)
		if string(payload[start:end]) != edit.Value.Raw {
			return payload, false
		}
		size += len(edit.Raw) - len(edit.Value.Raw)
	}
	out := make([]byte, 0, size)
	end = 0
	for _, edit := range edits {
		out = append(out, payload[end:edit.Value.Index]...)
		out = append(out, edit.Raw...)
		end = edit.Value.Index + len(edit.Value.Raw)
	}
	return append(out, payload[end:]...), true
}
