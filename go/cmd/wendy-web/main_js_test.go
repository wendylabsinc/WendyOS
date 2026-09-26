//go:build js && wasm

package main

import (
	"encoding/json"
	"strings"
	"testing"
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
