package backend

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/cloudrelay"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestVPNReportsUnsupportedCloudPortOnce(t *testing.T) {
	want := &cloudrelay.UnsupportedServiceError{Service: "tcp-9130"}
	var messages []string
	calls := 0
	dial := reportVPNDialFailures(func(_ context.Context, selector string, port uint16) (net.Conn, error) {
		calls++
		if selector != "cloud://selected" || port != 9130 {
			t.Fatal("selected route changed")
		}
		return nil, fmt.Errorf("dial failed: %w", want)
	}, func(message string) { messages = append(messages, message) })
	for range 3 {
		if _, err := dial(context.Background(), "cloud://selected", 9130); !errors.Is(err, want) {
			t.Fatalf("error was lost: %v", err)
		}
	}
	if calls != 3 || len(messages) != 1 || messages[0] != want.Error() {
		t.Fatalf("calls=%d, notifications=%v", calls, messages)
	}
}

func TestVPNDoesNotMisreportAuthorizationOrCancellation(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		var want error = status.Error(codes.PermissionDenied, "denied")
		if cancelled {
			cancel()
			want = &cloudrelay.UnsupportedServiceError{Service: "tcp-9130"}
		}
		dial := reportVPNDialFailures(func(context.Context, string, uint16) (net.Conn, error) { return nil, want },
			func(string) { t.Error("reported a missing service for a denied or cancelled dial") })
		if _, err := dial(ctx, "cloud://selected", 9130); err != want {
			t.Fatal("dial error changed")
		}
		cancel()
	}
}
