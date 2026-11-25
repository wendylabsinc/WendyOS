package mobile

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/macos/backend"
)

func TestABIValidatesBeforeDispatch(t *testing.T) {
	called := false
	c := &Client{ready: true, methods: map[string]any{"Change": func(session uint64, enabled bool) error { called = true; return nil }}}
	for _, input := range []string{
		`{"method":"Unknown","args":[]}`,
		`{"method":"Change","args":[1]}`,
		`{"method":"Change","args":[-1,true]}`,
		`{"method":"Change","args":[1,null]}`,
		`{"method":"Change","args":[1,"yes"]}`,
		`{"method":"Change","args":[1,true],"extra":true}`,
		`{"method":"Change","args":[1,true]} {}`,
		strings.Repeat(" ", maxRequestBytes+1),
	} {
		var out response
		if err := json.Unmarshal(c.Invoke([]byte(input)), &out); err != nil {
			t.Fatal(err)
		}
		if out.Error == "" || called {
			t.Fatalf("invalid input dispatched: %.150s", input)
		}
	}
	var out response
	json.Unmarshal(c.Invoke([]byte(`{"method":"Change","args":[42,true]}`)), &out)
	if !called || out.Error != "" {
		t.Fatalf("valid request failed: %+v", out)
	}
}
func TestABIPreservesErrorsAndUnicode(t *testing.T) {
	c := &Client{ready: true, methods: map[string]any{
		"Echo": func(s string) (string, error) { return s, nil },
		"Fail": func() (string, error) { return "partial result", errors.New("device disconnected") },
	}}
	var out response
	json.Unmarshal(c.Invoke([]byte(`{"method":"Echo","args":["Wendy 🤖 東京\u0000"]}`)), &out)
	if out.Value != "Wendy 🤖 東京\x00" {
		t.Fatalf("Unicode corrupted: %#v", out.Value)
	}
	json.Unmarshal(c.Invoke([]byte(`{"method":"Fail","args":[]}`)), &out)
	if out.Error != "device disconnected" {
		t.Fatal(out)
	}
}
func TestABIRequiresInitialization(t *testing.T) {
	var out response
	json.Unmarshal(New().Invoke([]byte(`{"method":"Nearby","args":[]}`)), &out)
	if out.Error != "Wendy has not initialized" {
		t.Fatal(out)
	}
}

func TestNearbySetupResultKeepsStateWithError(t *testing.T) {
	state := backend.NearbySetupState{Session: 7, Attempted: true, Pending: true}
	result := newNearbySetupResult(state, errors.New("device pki enrollment failed: i/o timeout"))
	if result.State.Session != 7 || !result.State.Attempted || !result.State.Pending {
		t.Fatalf("state was not preserved: %+v", result.State)
	}
	if result.Error == "" {
		t.Fatal("error text was dropped")
	}
	if newNearbySetupResult(state, nil).Error != "" {
		t.Fatal("success reported an error")
	}
}

// Connect carries the discovered device's mTLS requirement, as the iOS bridge does.
func TestConnectTakesTheMTLSRequirement(t *testing.T) {
	method := reflect.TypeOf(New().methods["Connect"])
	if method.NumIn() != 3 || method.In(0).Kind() != reflect.String || method.In(1).Kind() != reflect.Bool || method.In(2).Kind() != reflect.Bool {
		t.Fatalf("Connect is %v, want func(selector string, forget, requiresMTLS bool)", method)
	}
	if _, err := invoke(New().methods["Connect"], []json.RawMessage{json.RawMessage(`"pi.local:50051"`), json.RawMessage(`false`)}); err == nil {
		t.Fatal("a call without the mTLS requirement must be rejected")
	}
}
