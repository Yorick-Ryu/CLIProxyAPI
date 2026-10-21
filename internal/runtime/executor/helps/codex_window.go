package helps

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
)

// CodexWindowOrdinal prefers the canonical body metadata over compatibility
// projections, then falls back to the supplied window IDs. Missing identities
// are not synthesized; the caller decides whether to generate a window.
func CodexWindowOrdinal(body []byte, headers http.Header) string {
	metadata := gjson.GetBytes(body, "client_metadata.x-codex-turn-metadata")
	nested := gjson.Parse(metadata.String())
	var headerMetadata gjson.Result
	for key, values := range headers {
		if strings.EqualFold(key, "x-codex-turn-metadata") && len(values) > 0 {
			headerMetadata = gjson.Parse(values[0])
			break
		}
	}
	for _, value := range []gjson.Result{nested.Get("window_number"), gjson.GetBytes(body, "client_metadata.window_number"), headerMetadata.Get("window_number")} {
		if value.Type == gjson.Number {
			if number, err := strconv.ParseUint(value.Raw, 10, 64); err == nil {
				return strconv.FormatUint(number, 10)
			}
		}
	}
	windows := []string{nested.Get("window_id").String(), gjson.GetBytes(body, "client_metadata.x-codex-window-id").String(), gjson.GetBytes(body, "client_metadata.window_id").String(), headerMetadata.Get("window_id").String()}
	for key, values := range headers {
		if strings.EqualFold(key, "x-codex-window-id") && len(values) > 0 {
			windows = append(windows, values[0])
		}
	}
	for _, window := range windows {
		if index := strings.LastIndexByte(window, ':'); index > 0 {
			if number, err := strconv.ParseUint(window[index+1:], 10, 64); err == nil {
				return strconv.FormatUint(number, 10)
			}
		}
	}
	return "0"
}
