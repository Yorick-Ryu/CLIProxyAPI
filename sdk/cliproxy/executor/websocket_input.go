package executor

import (
	"context"
	"errors"
)

// ErrNoActiveUpstreamWebsocket means response.interrupt has no live upstream socket.
var ErrNoActiveUpstreamWebsocket = errors.New("no active upstream websocket for response.interrupt")

// ErrUpstreamWebsocketInterruptRejected means a live upstream socket owns the
// current turn, but response.interrupt does not target its running response
// (unknown id, or that response already finished). Handlers must reject the
// frame outright and must not fall back to local cancellation, which would
// cancel a healthy websocket turn.
var ErrUpstreamWebsocketInterruptRejected = errors.New("response.interrupt does not target the active upstream response")

// WebsocketInput is a frame from the single downstream reader. Err terminates
// the connection; Payload is owned by the receiver and must not be replayed.
type WebsocketInput struct {
	Payload []byte
	Err     error
	// Epoch is the downstream input epoch at read time; a loss advances it.
	Epoch uint64
}
type websocketInputKey struct{}

func WithWebsocketInput(ctx context.Context, input <-chan WebsocketInput) context.Context {
	return context.WithValue(ctx, websocketInputKey{}, input)
}
func WebsocketInputFromContext(ctx context.Context) <-chan WebsocketInput {
	if ctx == nil {
		return nil
	}
	input, _ := ctx.Value(websocketInputKey{}).(<-chan WebsocketInput)
	return input
}

// WithWebsocketAuthCheck supplies the live account-state check for a bound
// connection. It may reject further frames but never select another account.
type websocketAuthCheckKey struct{}

func WithWebsocketAuthCheck(ctx context.Context, check func(string) bool) context.Context {
	return context.WithValue(ctx, websocketAuthCheckKey{}, check)
}
func WebsocketAuthEnabled(ctx context.Context, authID string) bool {
	if ctx == nil {
		return true
	}
	check, _ := ctx.Value(websocketAuthCheckKey{}).(func(string) bool)
	return check == nil || check(authID)
}
