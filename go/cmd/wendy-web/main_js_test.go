//go:build js && wasm

package main

import (
	"encoding/json"
	"os"
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

func TestHeadAckVerify(t *testing.T) {
	raw, err := os.ReadFile("../../internal/shared/headack/testdata/device-signed-request-vectors-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Root    []byte `json:"root_cert_der_b64"`
		Vectors []struct {
			Name           string `json:"name"`
			Device         string `json:"device"`
			OrganizationID string `json:"organization_id"`
			Response       []byte `json:"response_b64"`
		} `json:"head_ack_read_vectors"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	b := &bridge{}
	call := func(name string, kept any) (any, error) {
		for _, v := range file.Vectors {
			if v.Name == name {
				params, _ := json.Marshal(map[string]any{"response": v.Response, "rootDER": file.Root, "device": v.Device, "organizationId": v.OrganizationID, "kept": kept})
				return b.handle(request{Method: "head-ack-verify", Params: params})
			}
		}
		t.Fatalf("no vector %s", name)
		return nil, nil
	}
	value, err := call("last-head-ack-valid", nil)
	if err != nil {
		t.Fatal(err)
	}
	// The page keeps the result as JSON and passes it back as kept.
	kept, _ := json.Marshal(value)
	var got struct {
		EntryID    int64     `json:"entryId"`
		ObservedAt time.Time `json:"observedAt"`
	}
	if err := json.Unmarshal(kept, &got); err != nil || got.EntryID != 42 || got.ObservedAt.Unix() != 1789999990 {
		t.Fatalf("verified %s, %v", kept, err)
	}
	if _, err := call("last-head-ack-valid", json.RawMessage(kept)); err != nil {
		t.Fatalf("same ack against its kept state: %v", err)
	}
	if _, err := call("head-below-ack-invalid", nil); err == nil || !strings.Contains(err.Error(), "head_below_ack") {
		t.Fatalf("head below ack: %v", err)
	}
	if _, err := call("never-acked-invalid", nil); err == nil || !strings.Contains(err.Error(), "never_acked") {
		t.Fatalf("never acked: %v", err)
	}
}
