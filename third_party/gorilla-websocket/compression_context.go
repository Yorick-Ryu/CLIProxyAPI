package websocket

import (
	"compress/flate"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	windowflate "github.com/klauspost/compress/flate"
)

// This client-only extension is opt-in. The server and default client retain
// upstream Gorilla's no-context-takeover behavior.
func (c *Conn) negotiateContextTakeover(headers http.Header) error {
	value := strings.TrimSpace(strings.Join(headers.Values("Sec-WebSocket-Extensions"), ","))
	if value == "" {
		return nil
	}
	name, rest := nextToken(value)
	if name != "permessage-deflate" {
		return errInvalidCompression
	}
	params := make(map[string]string)
	for rest = skipSpace(rest); rest != ""; rest = skipSpace(rest) {
		if rest[0] != ';' {
			return errInvalidCompression
		}
		var key, val string
		key, rest = nextToken(skipSpace(rest[1:]))
		if key == "" {
			return errInvalidCompression
		}
		if _, duplicate := params[key]; duplicate {
			return errInvalidCompression
		}
		rest = skipSpace(rest)
		hasValue := strings.HasPrefix(rest, "=")
		if hasValue {
			val, rest = nextTokenOrQuoted(skipSpace(rest[1:]))
			if val == "" {
				return errInvalidCompression
			}
		}
		switch key {
		case "client_no_context_takeover", "server_no_context_takeover":
			if hasValue {
				return errInvalidCompression
			}
		case "client_max_window_bits", "server_max_window_bits":
			bits, err := strconv.Atoi(val)
			if err != nil || bits < 8 || bits > 15 || strconv.Itoa(bits) != val {
				return errInvalidCompression
			}
		default:
			return errInvalidCompression
		}
		params[key] = val
	}
	bits := 15
	if val, ok := params["client_max_window_bits"]; ok {
		bits, _ = strconv.Atoi(val)
	}
	_, clientReset := params["client_no_context_takeover"]
	_, serverReset := params["server_no_context_takeover"]
	c.contextTakeover = true
	c.newCompressionWriter = contextCompressionWriter(bits, !clientReset)
	// A 32 KiB decoder also accepts streams produced with smaller windows.
	c.newDecompressionReader = contextDecompressionReader(!serverReset)
	return nil
}

type flushWriter interface {
	io.Writer
	Flush() error
}

func contextCompressionWriter(bits int, retain bool) func(io.WriteCloser, int) io.WriteCloser {
	var compressor flushWriter
	var sink truncWriter
	lastLevel := 0
	return func(w io.WriteCloser, level int) io.WriteCloser {
		sink = truncWriter{w: w}
		if compressor == nil || !retain || lastLevel != level {
			if bits == 15 {
				compressor, _ = flate.NewWriter(&sink, level)
			} else {
				// A negotiated small window must also limit encoded distances.
				compressor, _ = windowflate.NewWriterWindow(&sink, 1<<uint(bits))
			}
			lastLevel = level
		}
		return &contextWriteWrapper{compressor: compressor, sink: &sink}
	}
}

type contextWriteWrapper struct {
	compressor flushWriter
	sink       *truncWriter
	closed     bool
}

func (w *contextWriteWrapper) Write(p []byte) (int, error) {
	if w.closed {
		return 0, errWriteClosed
	}
	return w.compressor.Write(p)
}

func (w *contextWriteWrapper) Close() error {
	if w.closed {
		return errWriteClosed
	}
	w.closed = true
	if err := w.compressor.Flush(); err != nil {
		return err
	}
	if w.sink.n != 4 || w.sink.p != [4]byte{0, 0, 0xff, 0xff} {
		return errors.New("websocket: invalid context compression flush trailer")
	}
	return w.sink.w.Close()
}

func contextDecompressionReader(retain bool) func(io.Reader) io.ReadCloser {
	var history []byte
	return func(r io.Reader) io.ReadCloser {
		if !retain {
			history = history[:0]
		}
		const tail = "\x00\x00\xff\xff\x01\x00\x00\xff\xff"
		fr := flateReaderPool.Get().(io.ReadCloser)
		_ = fr.(flate.Resetter).Reset(io.MultiReader(r, strings.NewReader(tail)), history)
		return &contextReadWrapper{reader: fr, history: &history}
	}
}

type contextReadWrapper struct {
	reader  io.ReadCloser
	history *[]byte
	err     error
}

func (r *contextReadWrapper) Read(p []byte) (int, error) {
	if r.reader == nil {
		return 0, r.err
	}
	n, err := r.reader.Read(p)
	if n > 0 {
		const window = 32768
		history := *r.history
		if n >= window {
			history = append(history[:0], p[n-window:n]...)
		} else {
			if extra := len(history) + n - window; extra > 0 {
				copy(history, history[extra:])
				history = history[:len(history)-extra]
			}
			history = append(history, p[:n]...)
		}
		*r.history = history
	}
	if err != nil {
		r.err = err
		_ = r.reader.Close()
		flateReaderPool.Put(r.reader)
		r.reader = nil
	}
	return n, err
}

func (r *contextReadWrapper) Close() error {
	// NextReader may discard a partially consumed message. Inflate its remainder
	// before advancing so the next compressed message sees the complete history.
	if r.reader != nil {
		_, _ = io.Copy(io.Discard, r)
	}
	if r.err == io.EOF {
		return nil
	}
	return r.err
}
