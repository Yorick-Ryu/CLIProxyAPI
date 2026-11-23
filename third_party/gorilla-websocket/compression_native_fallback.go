//go:build !codex_zlib_rs || !cgo

package websocket

import "io"

func newNativeDeflateEncoder(int) (func(io.Writer, int) flushWriter, func()) {
	return nil, nil
}
