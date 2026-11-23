package websocket

import (
	"errors"
	"io"
)

type fragmentCompressor interface {
	io.WriteCloser
	flushFragment() error
}

// WriteMessageFragments writes a data message split into frames at the supplied
// uncompressed size. The optional callback runs immediately before writing each
// fragment. When compression is enabled, fragments share the message's DEFLATE
// stream. Control frames may be written between fragments via WriteControl.
func (c *Conn) WriteMessageFragments(messageType int, data []byte, fragmentSize int, beforeFragment func(index int)) (err error) {
	if !isData(messageType) || fragmentSize <= 0 {
		return errors.New("websocket: invalid data message fragmentation")
	}
	var mw messageWriter
	if err = c.beginMessage(&mw, messageType); err != nil {
		return err
	}
	mw.explicitFrame = c.writeBuf[:maxFrameHeaderSize]

	var writer io.WriteCloser = &mw
	var compressor fragmentCompressor
	if c.newCompressionWriter != nil && c.enableWriteCompression {
		compressor = c.newCompressionWriter(writer, c.compressionLevel).(fragmentCompressor)
		writer = compressor
		mw.compress = true
	}
	defer func() {
		if err != nil {
			mw.endMessage(c.writeFatal(err))
			if compressor != nil {
				// Return a pooled compressor without writing a final frame.
				_ = compressor.Close()
			}
		}
	}()

	for index := 0; ; index++ {
		if beforeFragment != nil {
			beforeFragment(index)
		}
		size := len(data)
		if size > fragmentSize {
			size = fragmentSize
		}
		if _, err = writer.Write(data[:size]); err != nil {
			return err
		}
		data = data[size:]
		if len(data) == 0 {
			err = writer.Close()
			compressor = nil // Close already returned the compressor to its pool.
			return err
		}
		if compressor != nil {
			if err = compressor.flushFragment(); err != nil {
				return err
			}
		}
		if err = mw.flushFrame(false, nil); err != nil {
			return err
		}
	}
}
