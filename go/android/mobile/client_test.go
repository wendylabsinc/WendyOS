package mobile

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
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
