package helps

import (
	"context"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

// PayloadFinalizer applies user rules to a fully prepared business payload.
// Each rebuilt attempt must start from the unconfigured body, not a previous result.
type PayloadFinalizer func([]byte) []byte

// NewPayloadFinalizer snapshots matching context before built-in request mutations.
func NewPayloadFinalizer(cfg *config.Config, executor, model, protocol, root string, original []byte, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) PayloadFinalizer {
	requestedModel := PayloadRequestedModel(opts, req.Model)
	requestPath := PayloadRequestPath(opts)
	headers := opts.Headers.Clone()
	var source []byte
	if payloadDefaultsMayMatch(cfg, model, requestedModel, protocol, opts.SourceFormat.String(), headers) {
		source = append([]byte(nil), original...)
	}
	var formatCallback func(string)
	if opts.Metadata != nil {
		formatCallback, _ = opts.Metadata[cliproxyexecutor.FinalizedResponseFormatCallbackMetadataKey].(func(string))
	}
	return func(body []byte) []byte {
		out := ApplyPayloadConfigWithRequestForExecutor(cfg, executor, model, protocol, opts.SourceFormat.String(), root, body, source, requestedModel, requestPath, headers)
		if formatCallback != nil {
			formatCallback(strings.TrimSpace(gjson.GetBytes(out, buildPayloadPath(root, "response_format")).String()))
		}
		return out
	}
}

// Only defaults consult the original payload. Body conditions must still be
// evaluated at the final barrier, since built-in mutations can change them.
func payloadDefaultsMayMatch(cfg *config.Config, model, requestedModel, protocol, fromProtocol string, headers http.Header) bool {
	if cfg == nil {
		return false
	}
	candidates := payloadModelCandidates(model, requestedModel)
	for _, rules := range [][]config.PayloadRule{cfg.Payload.Default, cfg.Payload.DefaultRaw} {
		for _, rule := range rules {
			if len(rule.Params) == 0 {
				continue
			}
			for _, entry := range rule.Models {
				entry.Match, entry.NotMatch, entry.Exist, entry.NotExist = nil, nil, nil, nil
				if payloadModelRulesMatch([]config.PayloadModelRule{entry}, protocol, fromProtocol, headers, nil, "", candidates) {
					return true
				}
			}
		}
	}
	return false
}

type payloadFinalizerKey struct{}

// WithPayloadFinalizer carries the final barrier through shared request builders.
func WithPayloadFinalizer(ctx context.Context, finalize PayloadFinalizer) context.Context {
	return context.WithValue(ctx, payloadFinalizerKey{}, finalize)
}

// FinalizePayload runs immediately before serialization or transport framing.
func FinalizePayload(ctx context.Context, body []byte) []byte {
	if finalize, ok := ctx.Value(payloadFinalizerKey{}).(PayloadFinalizer); ok && finalize != nil {
		return finalize(body)
	}
	return body
}
