package main

import (
	"context"
	"strings"
	"testing"
)

func TestReplayFlagsRequireExplicitPairBeforeOpeningCredentials(t *testing.T) {
	for _, args := range [][]string{
		{"--run-id", "evaluation", "--replay"},
		{"--run-id", "evaluation", "--recording-sha256", strings.Repeat("a", 64)},
		{"--run-id", "evaluation", "--replay", "--recording-sha256", "bad"},
	} {
		if err := run(context.Background(), args); err == nil || !strings.Contains(err.Error(), "must be supplied together") {
			t.Fatalf("unexpected result for %v: %v", args, err)
		}
	}
}
