package helps

import (
	"fmt"
	"sync"

	"github.com/klauspost/compress/zstd"
)

var codexRequestEncoders sync.Pool

// CompressCodexRequestBody matches the framing of Codex's
// zstd::stream::encode_all(Cursor::new(body), 3), without changing the JSON.
func CompressCodexRequestBody(body []byte) ([]byte, error) {
	encoder, _ := codexRequestEncoders.Get().(*zstd.Encoder)
	if encoder == nil {
		var err error
		encoder, err = zstd.NewWriter(nil,
			zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(3)),
			zstd.WithEncoderConcurrency(1),
			zstd.WithWindowSize(2<<20),
			zstd.WithEncoderCRC(false),
			zstd.WithSingleSegment(false),
			zstd.WithZeroFrames(true),
		)
		if err != nil {
			return nil, fmt.Errorf("create Codex zstd request encoder: %w", err)
		}
	}
	defer codexRequestEncoders.Put(encoder)
	compressed := encoder.EncodeAll(body, nil)
	if len(body) == 0 {
		// Native zstd emits a single-segment frame with a zero content size.
		return compressed, nil
	}

	// EncodeAll knows the input length and may shrink its advertised window.
	// Native streaming zstd does neither. Replace only the frame header; the
	// blocks already use at most our configured 2 MiB window and no checksum.
	var header zstd.Header
	if err := header.Decode(compressed); err != nil {
		return nil, fmt.Errorf("read Codex zstd request frame: %w", err)
	}
	const nativeHeader = "\x28\xb5\x2f\xfd\x00\x58"
	remaining := copy(compressed[len(nativeHeader):], compressed[header.HeaderSize:])
	copy(compressed, nativeHeader)
	return compressed[:len(nativeHeader)+remaining], nil
}
