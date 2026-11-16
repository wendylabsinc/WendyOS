package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/commands"
)

func TestTrackCommandCancellationIsNotAnErrorOrCompletion(t *testing.T) {
	for _, tc := range []struct {
		err   error
		class string
	}{
		{fmt.Errorf("interrupted: %w", context.Canceled), "context_canceled"},
		{commands.ErrUserCancelled, "user_cancelled"},
	} {
		t.Run(tc.class, func(t *testing.T) {
			events := captureAnalytics(t)
			root := &cobra.Command{Use: "wendy"}
			init := &cobra.Command{Use: "init", RunE: func(*cobra.Command, []string) error { return tc.err }}
			root.AddCommand(init)
			trackCommand(init, tc.err, time.Millisecond)
			for _, event := range *events {
				if event.event == "init_success" {
					t.Fatal("cancellation awarded a completion milestone")
				}
			}
			if len(*events) == 0 || (*events)[0].event != "command_executed" {
				t.Fatalf("missing command event: %v", *events)
			}
			props := (*events)[0].props
			if props["success"] != "true" || props["outcome"] != "cancelled" || props["error_class"] != tc.class {
				t.Fatalf("cancellation reported as a failure: %v", props)
			}
		})
	}
}
