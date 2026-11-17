package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"

	"github.com/wendylabsinc/wendy/go/internal/shared/resetcleanup"
)

// A device that was reset but cannot finish its cleanup refuses to report its
// enrollment state and to enroll. Whichever command meets that refusal, the
// user is told what is failing and what to do about it, once.
func TestResetCleanupRefusalSaysWhatFailedAndHowToRecover(t *testing.T) {
	const cause = `interface "wendy0" is not owned by wendy-net`
	for name, refusal := range map[string]error{
		"enrollment state": resetcleanup.Refusal(codes.Unavailable, "reset resource cleanup incomplete; agent recovery is retrying", errors.New(cause)),
		"enrollment":       resetcleanup.Refusal(codes.FailedPrecondition, "reset resource cleanup must finish before enrollment", errors.New(cause)),
	} {
		t.Run(name, func(t *testing.T) {
			err := fmt.Errorf("checking the device: %w", refusal)
			class := errorClass(err)
			env := newErrorEnvelope(err, class, outcomeForClass(class), nil)
			if !strings.Contains(env.Error.Message, cause) {
				t.Errorf("JSON message %q does not say what is failing", env.Error.Message)
			}
			steps := strings.Join(env.Error.NextSteps, " ")
			if !strings.Contains(steps, "restart the device") {
				t.Errorf("JSON next steps %q do not say how to recover", env.Error.NextSteps)
			}
			if strings.Contains(env.Error.Message, "restart the device") {
				t.Errorf("JSON message %q repeats the recovery step", env.Error.Message)
			}

			text := renderError(err)
			if !strings.Contains(text, cause) {
				t.Errorf("human output %q does not say what is failing", text)
			}
			if strings.Count(text, "restart the device") != 1 {
				t.Errorf("human output must show the recovery step once: %q", text)
			}
		})
	}

	// An agent that is simply not answering gets no such advice.
	if text := renderError(errors.New("rpc error: code = Unavailable desc = connection refused")); strings.Contains(text, "restart the device") {
		t.Errorf("an unrelated failure was given reset-cleanup advice: %q", text)
	}
}
