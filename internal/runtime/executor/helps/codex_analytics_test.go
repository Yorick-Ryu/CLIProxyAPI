package helps

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/tidwall/gjson"
)

func TestCodexAnalyticsMetadata(t *testing.T) {
	for _, tc := range []struct {
		name, metadata, want string
	}{
		{"enabled", ` {"analytics_enabled":true,"thread_id":"keep","custom":{"n":9007199254740993}} `, ` {"analytics_enabled":false,"thread_id":"keep","custom":{"n":9007199254740993}} `},
		{"already disabled", ` {"analytics_enabled":false} `, ` {"analytics_enabled":false} `},
		{"missing flag", `{"thread_id":"keep"}`, `{"thread_id":"keep","analytics_enabled":false}`},
		{"empty object", `{}`, `{"analytics_enabled":false}`},
		{"wrong flag type", `{"analytics_enabled":"true"}`, `{"analytics_enabled":false}`},
		{"malformed", `{"analytics_enabled":true`, `{"analytics_enabled":true`},
		{"array", `[{"analytics_enabled":true}]`, `[{"analytics_enabled":true}]`},
		{"null", `null`, `null`},
		{"scalar", `true`, `true`},
		{"empty", ``, ``},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{"client_metadata": map[string]any{"x-codex-turn-metadata": tc.metadata}, "input": "unchanged"})
			if err != nil {
				t.Fatal(err)
			}
			original := string(body)
			for _, cfg := range []*config.Config{nil, {}} {
				if got := ApplyCodexAnalyticsBody(cfg, body); string(got) != original {
					t.Fatal("default changed request bytes")
				}
			}
			cfg := &config.Config{Codex: config.CodexConfig{AnalyticsEnabled: new(false)}}
			got := ApplyCodexAnalyticsBody(cfg, body)
			if value := gjson.GetBytes(got, "client_metadata.x-codex-turn-metadata").String(); value != tc.want {
				t.Fatalf("metadata = %q, want %q", value, tc.want)
			}
			if string(body) != original || gjson.GetBytes(got, "input").String() != "unchanged" {
				t.Fatal("mutated caller payload or unrelated content")
			}
			headers := http.Header{"x-codex-turn-metadata": {tc.metadata, tc.metadata}, "X-Unrelated": {"keep"}}
			shared := headers.Clone()
			originalValues := headers["x-codex-turn-metadata"]
			ApplyCodexAnalyticsHeaders(nil, headers)
			ApplyCodexAnalyticsHeaders(&config.Config{}, headers)
			if !reflect.DeepEqual(headers, shared) {
				t.Fatal("default changed headers")
			}
			ApplyCodexAnalyticsHeaders(cfg, headers)
			if !reflect.DeepEqual(headers["x-codex-turn-metadata"], []string{tc.want, tc.want}) || headers.Get("X-Unrelated") != "keep" {
				t.Fatalf("unexpected headers: %v", headers)
			}
			if originalValues[0] != tc.metadata {
				t.Fatal("mutated shared header values")
			}
		})
	}
}

func TestCodexAnalyticsAbsentOrNonStringMetadata(t *testing.T) {
	cfg := &config.Config{Codex: config.CodexConfig{AnalyticsEnabled: new(false)}}
	for _, body := range []string{`{}`, `{"client_metadata":{}}`, `{"client_metadata":{"x-codex-turn-metadata":null}}`, `{"client_metadata":{"x-codex-turn-metadata":42}}`, `{"client_metadata":{"x-codex-turn-metadata":[]}}`, `{"client_metadata":`} {
		if got := ApplyCodexAnalyticsBody(cfg, []byte(body)); string(got) != body {
			t.Fatalf("changed absent or invalid metadata: %s", got)
		}
	}
	body := []byte(`{"client_metadata":{"x-codex-turn-metadata":{"analytics_enabled":true,"marker":"keep"}}}`)
	got := ApplyCodexAnalyticsBody(cfg, body)
	metadata := gjson.GetBytes(got, "client_metadata.x-codex-turn-metadata")
	if !metadata.IsObject() || metadata.Get("analytics_enabled").Type != gjson.False || metadata.Get("marker").String() != "keep" {
		t.Fatalf("object metadata changed type or lost fields: %s", got)
	}
	headers := http.Header{}
	ApplyCodexAnalyticsHeaders(cfg, headers)
	ApplyCodexAnalyticsHeaders(cfg, nil)
	if len(headers) != 0 {
		t.Fatal("created absent metadata header")
	}
}
