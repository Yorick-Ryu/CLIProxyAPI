//go:build codex_zlib_rs && cgo

package websocket

/*
#cgo LDFLAGS: ${SRCDIR}/codex-deflate/target/release/libcpa_codex_deflate.a
#cgo linux LDFLAGS: -ldl -lpthread -lm -lrt -lutil
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

typedef struct CpaDeflate CpaDeflate;

typedef struct {
	const uint8_t *data;
	size_t len;
} CpaDeflateOutput;

CpaDeflate *cpa_deflate_new(uint8_t bits, uint8_t level);
void cpa_deflate_free(CpaDeflate *encoder);
bool cpa_deflate_write(CpaDeflate *encoder, const uint8_t *data, size_t len);
CpaDeflateOutput cpa_deflate_flush(CpaDeflate *encoder);
*/
import "C"

import (
	"compress/flate"
	"errors"
	"io"
	"sync"
	"unsafe"
)

type nativeDeflateState struct {
	mu     sync.Mutex
	ptr    *C.CpaDeflate
	closed bool
}

func newNativeDeflateEncoder(bits int) (func(io.Writer, int) flushWriter, func()) {
	// flate2's raw compressor accepts window sizes from 9 to 15.
	if bits < 9 {
		return nil, nil
	}
	state := &nativeDeflateState{}
	return func(sink io.Writer, level int) flushWriter {
			state.mu.Lock()
			defer state.mu.Unlock()
			if state.ptr != nil {
				C.cpa_deflate_free(state.ptr)
				state.ptr = nil
			}
			if level == flate.HuffmanOnly {
				return nil
			}
			if level == flate.DefaultCompression {
				level = 6
			}
			if !state.closed {
				state.ptr = C.cpa_deflate_new(C.uint8_t(bits), C.uint8_t(level))
			}
			return &nativeDeflateWriter{state: state, sink: sink}
		}, func() {
			state.mu.Lock()
			defer state.mu.Unlock()
			state.closed = true
			if state.ptr != nil {
				C.cpa_deflate_free(state.ptr)
				state.ptr = nil
			}
		}
}

type nativeDeflateWriter struct {
	state *nativeDeflateState
	sink  io.Writer
}

func (w *nativeDeflateWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	w.state.mu.Lock()
	defer w.state.mu.Unlock()
	if w.state.closed {
		return 0, io.ErrClosedPipe
	}
	if !C.cpa_deflate_write(w.state.ptr, (*C.uint8_t)(unsafe.SliceData(p)), C.size_t(len(p))) {
		return 0, errors.New("websocket: zlib-rs fragment compression failed")
	}
	return len(p), nil
}

func (w *nativeDeflateWriter) Flush() error {
	w.state.mu.Lock()
	defer w.state.mu.Unlock()
	if w.state.closed {
		return io.ErrClosedPipe
	}
	result := C.cpa_deflate_flush(w.state.ptr)
	if result.data == nil {
		return errors.New("websocket: zlib-rs fragment flush failed")
	}
	_, err := w.sink.Write(unsafe.Slice((*byte)(result.data), int(result.len)))
	return err
}
