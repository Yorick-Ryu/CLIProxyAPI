package helps

import "testing"

func TestCodexSearchMetadataPreservesNativeMapLayout(t *testing.T) {
	input := `{"session_id":"s","thread_id":"s","reasoning_effort":"low","turn_id":"t","model":"m","extension":"测试😀","turn_started_at_unix_ms":9007199254740993,"codex_version":"0.163.0"}`
	want := `{"session_id":"s","thread_id":"s","reasoning_effort":"low","turn_id":"t","model":"m","extension":"\u6d4b\u8bd5\ud83d\ude00","turn_started_at_unix_ms":9007199254740993,"codex_version":"0.163.0"}`
	if got := CodexSearchTurnMetadata(input); got != want {
		t.Fatalf("metadata serialization differs: %s", got)
	}
	if got := CodexSearchTurnMetadata(want); got != want {
		t.Fatal("existing ASCII serialization changed")
	}
	for _, value := range []string{`not JSON`, "\xff", `{"a":`} {
		if CodexSearchTurnMetadata(value) != value {
			t.Fatal("invalid metadata changed")
		}
	}
}
