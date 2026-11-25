package unitree

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestConnectionFailureMessageDoesNotBlameAccountForLANFailures(t *testing.T) {
	for _, test := range []struct {
		err  error
		want string
	}{
		{errors.New("Unitree signaling is unreachable on ports 9991 and 8081; check the IP address and LAN connection"), "unreachable"},
		{errors.New("Unitree is busy; arrange an explicit handoff with the current robot operator before retrying"), "another controller"},
		{errors.New("Unitree rejected the encryption key; select the account robot that matches this IP address"), "same robot"},
		{fmt.Errorf("validate Unitree connection: %w", context.DeadlineExceeded), "timed out"},
	} {
		message := ConnectionFailureMessage(test.err)
		if !strings.Contains(message, test.want) || strings.Contains(message, "password") || strings.Contains(message, "sign in") {
			t.Fatalf("misleading failure: %s", message)
		}
	}
}
func TestConnectionFailureMessageRedactsUnknownPeerErrors(t *testing.T) {
	message := ConnectionFailureMessage(errors.New("peer echoed password-secret key-secret SDP-secret"))
	if strings.Contains(message, "secret") || !strings.Contains(message, "setup failed") {
		t.Fatalf("unsafe message: %s", message)
	}
}

func TestSafeConnectionFailureMessage(t *testing.T) {
	for cause, message := range connectionFailureMessages {
		t.Run(cause, func(t *testing.T) {
			if got := SafeConnectionFailureMessage(ConnectionFailureMessage(errors.New(cause))); got != message {
				t.Fatalf("known diagnostic = %q, want %q", got, message)
			}
			if got := SafeConnectionFailureMessage(message + " echoed-secret"); got != connectionFailureFallback {
				t.Fatalf("accepted extra remote text: %q", got)
			}
		})
	}
	for _, err := range []error{context.DeadlineExceeded, context.Canceled, errors.New("unknown-secret")} {
		message := ConnectionFailureMessage(err)
		if got := SafeConnectionFailureMessage(message); got != message {
			t.Fatalf("safe diagnostic = %q, want %q", got, message)
		}
	}
	for _, message := range []string{"", "echoed-secret", "rpc error: echoed-secret"} {
		if got := SafeConnectionFailureMessage(message); got != connectionFailureFallback {
			t.Fatalf("accepted arbitrary remote text: %q", got)
		}
	}
}
