package helps

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestCodexRequestCompressionNativeFrames(t *testing.T) {
	raw, err := os.ReadFile("testdata/codex_zstd_native.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Size     int    `json:"size"`
			FrameHex string `json:"frame_hex"`
		} `json:"cases"`
	}
	if err = json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, tc := range fixture.Cases {
		t.Run(fmt.Sprint(tc.Size), func(t *testing.T) {
			t.Parallel()
			native, errDecode := hex.DecodeString(tc.FrameHex)
			if errDecode != nil {
				t.Fatal(errDecode)
			}
			body := bytes.Repeat([]byte("x"), tc.Size)
			wire, errCompress := CompressCodexRequestBody(body)
			if errCompress != nil {
				t.Fatal(errCompress)
			}
			var nativeHeader, header zstd.Header
			if errDecode = nativeHeader.Decode(native); errDecode != nil {
				t.Fatal(errDecode)
			}
			if errDecode = header.Decode(wire); errDecode != nil {
				t.Fatal(errDecode)
			}
			if !bytes.Equal(wire[:header.HeaderSize], native[:nativeHeader.HeaderSize]) {
				t.Fatalf("frame header = %x, native = %x", wire[:header.HeaderSize], native[:nativeHeader.HeaderSize])
			}
			decoder, errDecoder := zstd.NewReader(nil)
			if errDecoder != nil {
				t.Fatal(errDecoder)
			}
			defer decoder.Close()
			for _, frame := range [][]byte{native, wire} {
				decoded, errFrame := decoder.DecodeAll(frame, nil)
				if errFrame != nil || !bytes.Equal(decoded, body) {
					t.Fatalf("frame does not round-trip: %v", errFrame)
				}
			}
		})
	}
}

func TestCodexRequestCompressionWindowAndIndependentBodies(t *testing.T) {
	// Repeats across the window boundary exercise actual backreferences,
	// including pooled encoders following a different request.
	body := make([]byte, 5<<20)
	rng := rand.New(rand.NewSource(1))
	_, _ = rng.Read(body[:3<<20])
	copy(body[3<<20:], body[1<<20:3<<20])
	decoder, err := zstd.NewReader(nil, zstd.WithDecoderMaxWindow(2<<20))
	if err != nil {
		t.Fatal(err)
	}
	defer decoder.Close()
	for _, input := range [][]byte{body, []byte(`{"input":"short"}`), body[:65536], nil, body} {
		wire, errCompress := CompressCodexRequestBody(input)
		if errCompress != nil {
			t.Fatal(errCompress)
		}
		decoded, errDecode := decoder.DecodeAll(wire, nil)
		if errDecode != nil || !bytes.Equal(decoded, input) {
			t.Fatalf("%d byte request does not round-trip within the native window: %v", len(input), errDecode)
		}
	}
}
