//go:build js && wasm

package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestRejectsUICertificateConnections(t *testing.T) {
	for _, params := range []json.RawMessage{
		nil,
		json.RawMessage(`{"relay":"wss://untrusted.example/tunnel","certificate":{"pemPrivateKey":"test-only-key","organizationId":1},"assetId":1}`),
	} {
		b := &bridge{}
		value, err := b.handle(request{Method: "connect", Params: params})
		if value != nil || err == nil || !strings.Contains(err.Error(), "Direct certificate connections are disabled") {
			t.Fatalf("legacy connection must be rejected: value=%v, err=%v", value, err)
		}
		if b.conn != nil || b.life != nil {
			t.Fatal("legacy connection created a device session")
		}
	}
}

func TestValidUntil(t *testing.T) {
	b := &bridge{}
	for params, want := range map[string]time.Duration{
		``:                             60 * 24 * time.Hour,
		`{}`:                           60 * 24 * time.Hour,
		`{"validForSeconds":3600}`:     time.Hour,
		`{"validForSeconds":31536000}`: 365 * 24 * time.Hour,
	} {
		before := time.Now()
		value, err := b.handle(request{Method: "valid-until", Params: json.RawMessage(params)})
		if err != nil {
			t.Fatalf("%q: %v", params, err)
		}
		got := time.Unix(value.(map[string]any)["seconds"].(int64), 0)
		if got.Before(before.Add(want).Truncate(time.Second)) || got.After(time.Now().Add(want)) {
			t.Errorf("%q: valid until %s, want now + %s", params, got, want)
		}
	}
	for _, params := range []string{`{"validForSeconds":31536001}`, `{"validForSeconds":-1}`, `{"validForSeconds":18446744074}`,
		`{"validForSeconds":-9223372036854775808}`, `{"validForSeconds":-18446744073}`, `{"validForSeconds":1.5}`, `{"validForSeconds":"3600"}`} {
		if _, err := b.handle(request{Method: "valid-until", Params: json.RawMessage(params)}); err == nil {
			t.Errorf("%s was accepted", params)
		}
	}
}
