package helps

import (
	"net/http"
	"testing"
	"time"
)

func TestCodexRetryAfterHeader(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, raw string
		want      time.Duration
		valid     bool
	}{
		{"seconds", "13", 13 * time.Second, true},
		{"whitespace", " 13 ", 13 * time.Second, true},
		{"zero", "0", 0, true},
		{"http_date", now.Add(17 * time.Second).Format(http.TimeFormat), 17 * time.Second, true},
		{"past_http_date", now.Add(-time.Second).Format(http.TimeFormat), 0, true},
		{"empty", "", 0, false},
		{"negative", "-1", 0, false},
		{"signed", "+13", 0, false},
		{"fractional", "0.5", 0, false},
		{"invalid", "tomorrow", 0, false},
		{"overflow", "9223372036854775807", 0, false},
		{"parse_overflow", "99999999999999999999999999999", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := CodexRetryAfterHeader(http.Header{"Retry-After": {tc.raw}}, now)
			if !tc.valid {
				if got != nil {
					t.Fatalf("got %v, want nil", *got)
				}
				return
			}
			if got == nil || *got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}
