package helps

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// IsNativeCodexRequest checks the client dialect for use inside Codex executors.
func IsNativeCodexRequest(body []byte, opts cliproxyexecutor.Options) bool {
	for _, format := range []sdktranslator.Format{opts.SourceFormat, cliproxyexecutor.ResponseFormatOrSource(opts)} {
		name := strings.TrimSpace(format.String())
		if !strings.EqualFold(name, sdktranslator.FormatCodex.String()) && !strings.EqualFold(name, sdktranslator.FormatOpenAIResponse.String()) {
			return false
		}
	}
	return util.IsCodexResponsesLiteRequest(body, opts.Headers)
}

// PreserveCodexInstructions recognizes native Responses clients independently of
// the Responses Lite output dialect. Empty native instructions are omitted on wire.
func PreserveCodexInstructions(body []byte, opts cliproxyexecutor.Options) bool {
	if IsNativeCodexRequest(body, opts) {
		return true
	}
	if opts.SourceFormat != sdktranslator.FormatCodex && opts.SourceFormat != sdktranslator.FormatOpenAIResponse {
		return false
	}
	return IsCodexUserAgent(opts.Headers)
}
