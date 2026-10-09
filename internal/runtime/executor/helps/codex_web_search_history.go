package helps

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// EnsureCodexWebSearchHistoryTool declares the hosted tool required to replay
// search history on the OAuth Responses endpoint. Call before built-in tool
// injection and before the final user payload rules; do not use for /compact.
func EnsureCodexWebSearchHistoryTool(body []byte, auth *cliproxyauth.Auth, headers http.Header) ([]byte, error) {
	if auth == nil || strings.TrimSpace(auth.Attributes["api_key"]) != "" || !bytes.Contains(body, []byte("web_search_call")) {
		return body, nil
	}
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body, nil
	}
	items := input.Array()
	hasHistory, callerTools := false, false
	additionalIndex := -1
	containsSearch := func(tools gjson.Result) bool {
		if !tools.IsArray() {
			return false
		}
		for _, tool := range tools.Array() {
			switch strings.TrimSpace(tool.Get("type").String()) {
			case "web_search", "web_search_preview", "web_search_preview_2025_03_11":
				return true
			}
		}
		return false
	}
	topTools := gjson.GetBytes(body, "tools")
	if containsSearch(topTools) {
		return body, nil
	}
	callerTools = topTools.IsArray() && len(topTools.Array()) > 0
	for i, item := range items {
		switch item.Get("type").String() {
		case "web_search_call":
			hasHistory = true
		case "additional_tools":
			tools := item.Get("tools")
			if containsSearch(tools) {
				return body, nil
			}
			callerTools = callerTools || (tools.IsArray() && len(tools.Array()) > 0)
			if additionalIndex < 0 {
				additionalIndex = i
			}
		}
	}
	if !hasHistory {
		return body, nil
	}
	const tool = `{"type":"web_search","external_web_access":false}`
	var next []byte
	var err error
	switch {
	case !util.IsCodexResponsesLiteRequest(body, headers):
		if topTools.IsArray() {
			next, err = sjson.SetRawBytes(body, "tools.-1", []byte(tool))
		} else {
			next, err = sjson.SetRawBytes(body, "tools", []byte("["+tool+"]"))
		}
	case additionalIndex >= 0:
		path := fmt.Sprintf("input.%d.tools", additionalIndex)
		if items[additionalIndex].Get("tools").IsArray() {
			next, err = sjson.SetRawBytes(body, path+".-1", []byte(tool))
		} else {
			next, err = sjson.SetRawBytes(body, path, []byte("["+tool+"]"))
		}
	default:
		at := len(items)
		if at > 0 && items[at-1].Get("type").String() == "compaction_trigger" {
			at--
		}
		values := make([]json.RawMessage, 0, len(items)+1)
		for i := 0; i <= len(items); i++ {
			if i == at {
				values = append(values, json.RawMessage(`{"type":"additional_tools","role":"developer","tools":[`+tool+`]}`))
			}
			if i < len(items) {
				values = append(values, json.RawMessage(items[i].Raw))
			}
		}
		var encoded []byte
		encoded, err = json.Marshal(values)
		if err == nil {
			next, err = sjson.SetRawBytes(body, "input", encoded)
		}
	}
	if err != nil {
		return body, fmt.Errorf("declare Codex search history tool: %w", err)
	}
	if !callerTools {
		choice := gjson.GetBytes(next, "tool_choice")
		normalized := strings.ToLower(strings.TrimSpace(choice.String()))
		if !choice.Exists() || choice.Type == gjson.Null || (choice.Type == gjson.String && (normalized == "" || normalized == "auto" || normalized == "none")) {
			next, err = sjson.SetBytes(next, "tool_choice", "none")
			if err != nil {
				return body, fmt.Errorf("preserve Codex no-tool choice: %w", err)
			}
		}
	}
	return next, nil
}
