package commands

import (
	"context"
	"errors"
	"testing"
)

func TestBuildStopsAfterUserRejectsReplacementTarget(t *testing.T) {
	connectionFailure := errors.New("device unreachable")
	for _, tc := range []struct {
		name string
		err  error
		stop bool
	}{
		{"ordinary unreachable device", connectionFailure, false},
		{"picker cancelled", ErrUserCancelled, true},
		{"command interrupted", context.Canceled, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := mustStopBuildAfterTargetError(tc.err); got != tc.stop {
				t.Fatalf("mustStopBuildAfterTargetError(%v) = %t, want %t", tc.err, got, tc.stop)
			}
		})
	}
}
