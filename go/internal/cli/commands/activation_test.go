package commands

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/analytics"
)

func TestActivationOperationRecordsCancellationAndFailedStep(t *testing.T) {
	for _, tc := range []struct {
		name           string
		err            error
		outcome, class string
	}{
		{"complete", nil, "completed", ""}, {"declined erase", ErrUserCancelled, "cancelled", "user_cancelled"},
		{"signal", context.Canceled, "cancelled", "context_canceled"},
		{"timeout", context.DeadlineExceeded, "failed", "context_deadline"},
		{"failure", errors.New("private token should not be included"), "failed", "other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var events []string
			var props []map[string]string
			analytics.SetTrackHookForTesting(func(event string, p map[string]string) { events = append(events, event); props = append(props, p) })
			t.Cleanup(func() { analytics.SetTrackHookForTesting(nil) })
			ctx, finish := startActivationOperation(context.Background(), "install", "wendy install")
			activationStep(ctx, "download", nil)
			finish(tc.err)
			if len(events) != 3 || events[0] != "install_started" || events[2] != "install_finished" {
				t.Fatalf("events %v", events)
			}
			last := props[2]
			if props[0]["success"] != "true" || props[1]["success"] != "true" {
				t.Fatalf("start/progress reported as errors: %v", props)
			}
			if last["success"] != strconv.FormatBool(tc.outcome != "failed") {
				t.Fatalf("terminal failure flag disagrees with outcome: %v", last)
			}
			if strings.Contains(last["error_detail"], "private token") {
				t.Fatal("arbitrary error text escaped")
			}
			if tc.err != nil && (last["error_code"] == "" || last["error_detail"] == "") {
				t.Fatal("failure has no structured remediation")
			}
			if last["outcome"] != tc.outcome || last["error_class"] != tc.class || last["step"] != "download" {
				t.Fatalf("wrong terminal diagnostic: %v", last)
			}
			if props[0]["attempt_id"] != last["attempt_id"] {
				t.Fatal("attempt changed during install")
			}
		})
	}
}

func TestActivationAppStartedIsImmediateAndOnce(t *testing.T) {
	var events []string
	analytics.SetTrackHookForTesting(func(event string, _ map[string]string) { events = append(events, event) })
	t.Cleanup(func() { analytics.SetTrackHookForTesting(nil) })
	m := newDeployMetrics(runOptions{})
	m.transport = "chunk"
	m.markStarted()
	m.markStarted()
	if len(events) != 1 || events[0] != "app_started" {
		t.Fatalf("start not reported immediately/once: %v", events)
	}
	m.emit(context.Canceled)
	if events[len(events)-1] != "deploy_completed" {
		t.Fatalf("completion missing: %v", events)
	}
}
