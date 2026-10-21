package helps

import (
	"net/http"
	"testing"
)

func TestCodexWindowOrdinalProjectionPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		headers http.Header
		want    string
	}{
		{"canonical number", `{"client_metadata":{"x-codex-turn-metadata":"{\"window_number\":3,\"window_id\":\"thread:0\"}","x-codex-window-id":"thread:0"}}`, nil, "3"},
		{"body window", `{"client_metadata":{"x-codex-window-id":"thread:17"}}`, nil, "17"},
		{"header window", `{}`, http.Header{"x-codex-window-id": {"thread:8"}}, "8"},
		{"header metadata", `{}`, http.Header{"X-Codex-Turn-Metadata": {`{"window_number":11}`}}, "11"},
		{"maximum integer", `{"client_metadata":{"x-codex-turn-metadata":{"window_number":18446744073709551615}}}`, nil, "18446744073709551615"},
		{"invalid number", `{"client_metadata":{"window_number":-1,"x-codex-window-id":"thread:2"}}`, nil, "2"},
		{"missing", `{}`, nil, "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := CodexWindowOrdinal([]byte(tc.body), tc.headers); got != tc.want {
				t.Fatalf("ordinal=%q, want %q", got, tc.want)
			}
		})
	}
}
