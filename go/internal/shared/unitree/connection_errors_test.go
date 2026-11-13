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
