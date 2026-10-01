package config

import (
	"bytes"
	"reflect"
	"testing"
)

func TestV8MigrationPreservesForkCodexPolicies(t *testing.T) {
	raw := []byte(`codex:
  timezone:
    mode: fixed
    zone: Asia/Singapore
  websockets-default: false
  websocket-max-message-bytes: 17416099
  identity-convergence: true
  identity-confuse: true
  request-compression:
    enabled: true
    min-bytes: 65536
codex-api-key:
  - api-key: test-key
    base-url: https://example.com
    websockets: false
  - api-key: inherited-key
    base-url: https://example.com
`)
	before, err := ParseConfigBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	readOnly, changed, err := NormalizeConfigLayout(raw, false)
	if err != nil || changed || !bytes.Equal(raw, readOnly) {
		t.Fatalf("legacy read changed configuration: changed=%v error=%v", changed, err)
	}
	migrated, changed, err := NormalizeConfigLayout(raw, true)
	if err != nil || !changed {
		t.Fatalf("migration: changed=%v error=%v", changed, err)
	}
	if err := ValidateV8Config(migrated); err != nil {
		t.Fatalf("migrated fork configuration is invalid: %v", err)
	}
	after, err := ParseConfigBytes(migrated)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before.Codex, after.Codex) || !reflect.DeepEqual(before.CodexKey, after.CodexKey) {
		t.Fatal("v8 migration changed Codex policies or per-key transport inheritance")
	}
	if after.CodexWebsocketsDefault() || after.CodexKey[0].Websockets == nil || *after.CodexKey[0].Websockets || after.CodexKey[1].Websockets != nil {
		t.Fatal("transport override or inheritance was lost")
	}
}
