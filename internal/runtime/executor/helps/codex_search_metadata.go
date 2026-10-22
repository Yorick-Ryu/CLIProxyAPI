package helps

import (
	"strings"
	"unicode/utf8"

	"github.com/tidwall/gjson"
)

// CodexSearchTurnMetadata retains the native extension map's field order.
// Search metadata is not serialized using the Responses struct layout. Only
// non-ASCII characters are escaped, matching Codex's to_ascii_json_string.
func CodexSearchTurnMetadata(raw string) string {
	if !utf8.ValidString(raw) || !gjson.Valid(raw) {
		return raw
	}
	var output strings.Builder
	output.Grow(len(raw))
	for _, character := range raw {
		if character <= 0x7f {
			output.WriteRune(character)
			continue
		}
		var escaped strings.Builder
		writeCodexMetadataString(&escaped, string(character))
		value := escaped.String()
		output.WriteString(value[1 : len(value)-1])
	}
	return output.String()
}
