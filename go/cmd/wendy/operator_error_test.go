package main

import (
	"fmt"
	"strings"
	"testing"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestOperatorRecoveryJSONIsRetryableWithNextSteps(t *testing.T) {
	st, err := status.New(codes.Unavailable, "local operator router has no authenticated peers").WithDetails(&errdetails.ErrorInfo{
		Domain: "wendy.sessionbroker", Reason: "OPERATOR_NO_PEERS",
	})
	if err != nil {
		t.Fatal(err)
	}
	err = fmt.Errorf("getting agent version: %w", st.Err())
	class := errorClass(err)
	env := newErrorEnvelope(err, class, outcomeForClass(class), nil)
	if !env.Error.Retryable || env.Error.Exit != exitUnreachable || len(env.Error.NextSteps) == 0 {
		t.Fatalf("local reconnect should be actionable and retryable: %+v", env.Error)
	}
	if !strings.Contains(env.Error.Message, "no authenticated peers") || !strings.Contains(strings.Join(env.Error.NextSteps, " "), "first-hop board") {
		t.Fatalf("local router cause or first-hop recovery missing: %+v", env.Error)
	}
	text := renderError(err)
	for _, step := range env.Error.NextSteps {
		if strings.Count(text, step) != 1 {
			t.Fatalf("human output must show recovery once: %q", text)
		}
	}
	for _, code := range []codes.Code{codes.PermissionDenied, codes.Unauthenticated} {
		err := status.Error(code, "peer rejected authentication")
		class := errorClass(err)
		env := newErrorEnvelope(err, class, outcomeForClass(class), nil)
		if env.Error.Retryable {
			t.Fatalf("authentication failure became retryable: %+v", env.Error)
		}
	}
}
