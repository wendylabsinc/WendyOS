package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/commands"
)

func TestDesktopInstallerCancellationIsNotSuccess(t *testing.T) {
	t.Setenv("WENDY_TELEMETRY_ENTRYPOINT", "")
	t.Setenv("WENDY_DESKTOP_INSTALLER", "1")
	for _, err := range []error{commands.ErrUserCancelled, fmt.Errorf("install: %w", commands.ErrUserCancelled)} {
		var out strings.Builder
		if got := reportFailure(&out, err, nil, []string{"install"}); got != 130 {
			t.Fatalf("cancelled desktop install exit = %d, want 130", got)
		}
		if out.Len() != 0 {
			t.Fatalf("cancellation printed an error: %s", out.String())
		}
	}
	if got := reportFailure(&strings.Builder{}, nil, nil, []string{"install"}); got != 0 {
		t.Fatalf("successful desktop install exit = %d", got)
	}
}
