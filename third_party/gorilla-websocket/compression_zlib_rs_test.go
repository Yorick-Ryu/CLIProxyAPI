//go:build codex_zlib_rs && cgo

package websocket

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"testing"
)

func TestZlibRSFragmentBytes(t *testing.T) {
	encode, release := contextCompressionWriter(15, true)
	defer release()

	prewarm := []byte("{\"type\":\"response.create\",\"generate\":false}")
	large := append([]byte("{\"input\":\""), bytes.Repeat([]byte("abc \\\\ repeated escape \\\\ "), 48000)...)
	large = append(large, '"', '}')

	// These hashes come from the pinned encoder with Codex's sync-flush loop.
	var wire compressionBuffer
	writer := encode(&wire, 6)
	if _, err := writer.Write(prewarm); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	assertFragmentHash(t, wire.Next(wire.Len()), "d689b878efaa32f81b0b1bfb101f4fbee74ed2e6eae5193488324a8cc34430ca")

	const fragmentSize = 1 << 20
	fragmented := encode(&wire, 6).(fragmentCompressor)
	if _, err := fragmented.Write(large[:fragmentSize]); err != nil {
		t.Fatal(err)
	}
	if err := fragmented.flushFragment(); err != nil {
		t.Fatal(err)
	}
	assertFragmentHash(t, wire.Next(wire.Len()), "3e31ac17d0d9a73bc71132bb8d5af82b5c1374a11a0f788d08fbf2f8be12e339")

	if _, err := fragmented.Write(large[fragmentSize:]); err != nil {
		t.Fatal(err)
	}
	if err := fragmented.Close(); err != nil {
		t.Fatal(err)
	}
	assertFragmentHash(t, wire.Bytes(), "b750440965ff2f40e3ee354154cfc0ea1818ac4f9bf273e83cd1ec7b709d2e5c")
}

func assertFragmentHash(t *testing.T, compressed []byte, want string) {
	t.Helper()
	if got := fmt.Sprintf("%x", sha256.Sum256(compressed)); got != want {
		t.Fatalf("compressed %d-byte fragment hash = %s, want %s", len(compressed), got, want)
	}
}
