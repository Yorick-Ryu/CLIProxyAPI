package openai

import (
	"errors"
	"net/http"

	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

// Only self-contained input can be replayed without the native upstream session.
func responsesWebsocketHasFullInput(payload []byte) bool {
	if responsesWebsocketRequestRequiresCurrentUpstream(payload) {
		return false
	}
	input := gjson.GetBytes(payload, "input")
	if !input.IsArray() {
		return false
	}
	for _, item := range input.Array() {
		if !item.IsObject() || item.Get("type").String() == "item_reference" {
			return false
		}
		if item.Get("id").Exists() && len(item.Map()) == 1 {
			return false
		}
	}
	return true
}

func isResponsesWebsocketSizeError(err error) bool {
	if err == nil {
		return false
	}
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) && closeErr.Code == websocket.CloseMessageTooBig {
		return true
	}
	var statusErr interface{ StatusCode() int }
	return errors.As(err, &statusErr) && statusErr.StatusCode() == http.StatusRequestEntityTooLarge &&
		gjson.Get(err.Error(), "error.code").String() == "message_too_big"
}
