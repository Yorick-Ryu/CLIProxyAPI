package helps

import (
	"encoding/binary"
	"strings"

	"github.com/google/uuid"
)

// CodexIdentityUUID scopes an identity deterministically while retaining the
// native UUID version and creation time for session, thread and turn IDs.
func CodexIdentityUUID(name, kind, source string) string {
	id := uuid.NewSHA1(uuid.NameSpaceOID, []byte(name))
	id[6] = (id[6] & 0x0f) | 0x40
	if kind != "session" && kind != "thread" && kind != "turn" && kind != "context-window" {
		return id.String()
	}

	if original, err := uuid.Parse(strings.TrimSpace(source)); err == nil && original.Version() == 7 && original.Variant() == uuid.RFC4122 {
		copy(id[:6], original[:6])
	} else {
		// Legacy IDs and account-wide convergence have no recoverable creation
		// time. Use a stable past anchor rather than a changing clock or future
		// timestamp derived from unrestricted hash bytes.
		const anchorMillis uint64 = 1767225600000 // 2026-01-01 UTC.
		const spanMillis uint64 = 180 * 24 * 60 * 60 * 1000
		millis := anchorMillis - binary.BigEndian.Uint64(id[:8])%spanMillis
		for index := 5; index >= 0; index-- {
			id[index] = byte(millis)
			millis >>= 8
		}
	}
	id[6] = (id[6] & 0x0f) | 0x70
	return id.String()
}
