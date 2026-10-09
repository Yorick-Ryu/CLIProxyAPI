package helps

import (
	"bytes"
	"net/http"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func TestEnsureCodexWebSearchHistoryTool(t *testing.T) {
	const history = `{"type":"web_search_call","id":"ws_test","status":"completed","action":{"type":"search","query":"example"},"annotations":[{"opaque":"keep"}]}`
	cases := []struct {
		name, body, choice      string
		lite, unchanged, apiKey bool
	}{
		{"standard empty", `{"input":[` + history + `],"tools":[],"tool_choice":"auto"}`, "none", false, false, false},
		{"missing", `{"input":[` + history + `]}`, "none", false, false, false},
		{"null", `{"input":[` + history + `],"tools":null,"tool_choice":null}`, "none", false, false, false},
		{"explicit required", `{"input":[` + history + `],"tool_choice":"required"}`, "required", false, false, false},
		{"function named search", `{"input":[` + history + `],"tools":[{"type":"function","name":"web_search"}],"tool_choice":"auto"}`, "auto", false, false, false},
		{"existing search", `{"input":[` + history + `],"tools":[{"type":"web_search","external_web_access":true}]}`, "", false, true, false},
		{"preview", `{"input":[` + history + `],"tools":[{"type":"web_search_preview"}]}`, "", false, true, false},
		{"nested search", `{"input":[` + history + `,{"type":"additional_tools","tools":[{"type":"web_search"}]}]}`, "", true, true, false},
		{"no history", `{"input":[{"role":"user","content":"web_search_call"}],"tools":[]}`, "", false, true, false},
		{"string input", `{"input":"web_search_call"}`, "", false, true, false},
		{"API key", `{"input":[` + history + `],"tools":[]}`, "", false, true, true},
		{"lite trigger", `{"input":[` + history + `,{"type":"compaction_trigger"}],"tools":[]}`, "none", true, false, false},
		{"lite extend", `{"input":[` + history + `,{"type":"additional_tools","role":"developer","tools":[{"type":"function","name":"test"}]},{"type":"compaction_trigger"}],"tool_choice":"auto"}`, "auto", true, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			auth := &cliproxyauth.Auth{Provider: "codex"}
			if tc.apiKey {
				auth.Attributes = map[string]string{"api_key": "synthetic"}
			}
			headers := http.Header{}
			if tc.lite {
				headers.Set("X-OpenAI-Internal-Codex-Responses-Lite", "true")
			}
			original := []byte(tc.body)
			backup := bytes.Clone(original)
			got, err := EnsureCodexWebSearchHistoryTool(original, auth, headers)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(original, backup) {
				t.Fatal("mutated caller buffer")
			}
			if tc.unchanged {
				if !bytes.Equal(got, original) {
					t.Fatal("changed excluded request")
				}
				return
			}
			if gjson.GetBytes(got, "tool_choice").String() != tc.choice {
				t.Fatalf("tool choice: %s", got)
			}
			var tools gjson.Result
			if tc.lite {
				tools = gjson.GetBytes(got, `input.#(type=="additional_tools").tools`)
				if gjson.GetBytes(got, `tools.#(type=="web_search")`).Exists() {
					t.Fatal("Lite top-level hosted tool")
				}
				items := gjson.GetBytes(got, "input").Array()
				if items[len(items)-1].Get("type").String() != "compaction_trigger" {
					t.Fatal("trigger moved")
				}
			} else {
				tools = gjson.GetBytes(got, "tools")
			}
			found := false
			for _, tool := range tools.Array() {
				if tool.Get("type").String() == "web_search" {
					found = true
					if tool.Get("external_web_access").Bool() {
						t.Fatal("external search enabled")
					}
				}
			}
			if !found {
				t.Fatal("missing hosted search declaration")
			}
			if gjson.GetBytes(got, `input.#(type=="web_search_call")`).Raw != history {
				t.Fatal("search history changed")
			}
			again, err := EnsureCodexWebSearchHistoryTool(got, auth, headers)
			if err != nil || !bytes.Equal(again, got) {
				t.Fatal("not idempotent")
			}
		})
	}
}
