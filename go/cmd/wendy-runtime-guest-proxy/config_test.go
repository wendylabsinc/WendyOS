package main

import "testing"

func TestParseForwardSpecs(t *testing.T) {
	got, err := parseForwardSpecs([]string{
		"6237=/run/buildkit/buildkitd.sock",
		"6238=/run/containerd/containerd.sock",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].port != 6237 || got[1].path != "/run/containerd/containerd.sock" {
		t.Fatalf("parseForwardSpecs = %#v", got)
	}
}

func TestParseForwardSpecsRejectsUnsafeOrAmbiguousValues(t *testing.T) {
	for _, values := range [][]string{
		nil,
		{"0=/run/a.sock"},
		{"6237=relative.sock"},
		{"6237=/run/../tmp/a.sock"},
		{"6237=/run/a.sock", "6237=/run/b.sock"},
	} {
		if _, err := parseForwardSpecs(values); err == nil {
			t.Errorf("parseForwardSpecs(%v) = nil error", values)
		}
	}
}
