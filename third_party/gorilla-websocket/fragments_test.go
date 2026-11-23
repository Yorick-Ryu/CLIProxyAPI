package websocket

import (
	"bytes"
	"testing"
)

func TestFragmentedMessagesRetainCompressionContext(t *testing.T) {
	var wire bytes.Buffer
	client := newTestConn(nil, &wire, false)
	client.newCompressionWriter, client.releaseCompressionWriter = contextCompressionWriter(15, true)
	defer client.Close()
	client.enableWriteCompression = true
	server := newTestConn(&wire, nil, true)
	server.newDecompressionReader = contextDecompressionReader(true)

	const fragmentSize = 1024 * 1024
	input := bytes.Repeat([]byte("shared dictionary across fragments and messages"), 66000)
	for i := 0; i < 2; i++ {
		if errWrite := client.WriteMessageFragments(TextMessage, input, fragmentSize, nil); errWrite != nil {
			t.Fatal(errWrite)
		}
		messageType, output, errRead := server.ReadMessage()
		if errRead != nil || messageType != TextMessage || !bytes.Equal(output, input) {
			t.Fatalf("message %d failed to decode: type %d, bytes %d, error %v", i+1, messageType, len(output), errRead)
		}
	}
}
