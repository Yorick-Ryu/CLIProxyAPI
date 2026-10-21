package helps

import (
	"bytes"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestCodexIdentityUUIDPreservesV7Timestamp(t *testing.T) {
	for _, source := range []string{
		"019a0992-1400-7abc-8def-0123456789ab",
		"019A0992-1400-7ABC-8DEF-0123456789AB",
		"00000000-0000-7abc-8def-0123456789ab",
		"ffffffff-ffff-7abc-8def-0123456789ab",
	} {
		for _, kind := range []string{"session", "thread", "turn"} {
			t.Run(kind+"/"+source, func(t *testing.T) {
				name := "caller-a:account-a:" + kind + ":" + source
				mapped := uuid.MustParse(CodexIdentityUUID(name, kind, source))
				original := uuid.MustParse(source)
				if mapped.Version() != 7 || mapped.Variant() != uuid.RFC4122 || !bytes.Equal(mapped[:6], original[:6]) {
					t.Fatalf("mapped UUID %s lost the source version, variant or timestamp", mapped)
				}
				if mapped == original || mapped.String() != CodexIdentityUUID(name, kind, source) {
					t.Fatal("mapping must anonymize the source deterministically")
				}
				if mapped.String() == CodexIdentityUUID("other-caller:"+name, kind, source) {
					t.Fatal("different identity scopes must remain isolated")
				}
			})
		}
	}
}

func TestCodexIdentityUUIDLegacyFallbackAndInstallation(t *testing.T) {
	upper := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	lower := upper.AddDate(0, 0, -180)
	for _, source := range []string{"", "legacy-session", "not-a-uuid", "ab976dda-8a27-4325-9fc2-2ae5bced3634", "019a0992-1400-7abc-cdef-0123456789ab"} {
		name := "legacy-scope:" + source
		mapped := uuid.MustParse(CodexIdentityUUID(name, "session", source))
		seconds, nanos := mapped.Time().UnixTime()
		created := time.Unix(seconds, nanos)
		if mapped.Version() != 7 || mapped.Variant() != uuid.RFC4122 || created.Before(lower) || created.After(upper) {
			t.Fatalf("legacy source %q produced an invalid fallback %s (%s)", source, mapped, created)
		}
		if mapped.String() != CodexIdentityUUID(name, "session", source) {
			t.Fatal("legacy fallback must not depend on the current clock")
		}
	}
	if got := CodexIdentityUUID("device-identity-fixture", "installation", "019a0992-1400-7abc-8def-0123456789ab"); got != "ab976dda-8a27-4325-9fc2-2ae5bced3634" {
		t.Fatalf("installation mapping changed: %s", got)
	}
}
