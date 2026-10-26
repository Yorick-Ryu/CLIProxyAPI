package helps

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// CodexRetryAfterHeader accepts the standard delay-seconds and HTTP-date forms.
// Reject overflowing delays rather than wrapping them into an early retry.
func CodexRetryAfterHeader(headers http.Header, now time.Time) *time.Duration {
	raw := strings.TrimSpace(headers.Get("Retry-After"))
	if raw == "" {
		return nil
	}
	digits := true
	for _, r := range raw {
		if r < '0' || r > '9' {
			digits = false
			break
		}
	}
	if digits {
		seconds, err := strconv.ParseUint(raw, 10, 64)
		if err != nil || seconds > uint64((1<<63-1)/int64(time.Second)) {
			return nil
		}
		delay := time.Duration(seconds) * time.Second
		return &delay
	}
	if deadline, err := http.ParseTime(raw); err == nil {
		delay := deadline.Sub(now)
		if delay < 0 {
			delay = 0
		}
		return &delay
	}
	return nil
}
