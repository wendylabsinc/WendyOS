package commands

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type deviceAddresser interface{ DeviceAddress() string }

func TestDeviceDialErrorKeepsMessageAndClassifiesTransportFailures(t *testing.T) {
	refused := status.Error(codes.Unavailable, "connection refused")
	err := newDeviceDialError("127.0.0.1:1", refused)

	if err.Error() != refused.Error() {
		t.Errorf("Error() = %q, want the cause's text %q unchanged", err.Error(), refused.Error())
	}
	if got := status.Code(err); got != codes.Unavailable {
		t.Errorf("status.Code = %v, want Unavailable", got)
	}
	var dial deviceAddresser
	if !errors.As(fmt.Errorf("connecting: %w", err), &dial) || dial.DeviceAddress() != "127.0.0.1:1" {
		t.Errorf("DeviceAddress not reachable through wrapping: %v", dial)
	}
	if got := ErrorClass(err); got != "device_unreachable" {
		t.Errorf("ErrorClass = %q, want device_unreachable", got)
	}

	// A non-transport failure is not called unreachable, and a more specific
	// verdict keeps its own class.
	if got := ErrorClass(newDeviceDialError("h:1", status.Error(codes.PermissionDenied, "no"))); got != "" {
		t.Errorf("PermissionDenied classified as %q, want unclassified", got)
	}
	if got := ErrorClass(newDeviceDialError("h:1", newProvisionedAgentUnauthorizedError(refused))); got != "device_auth_required" {
		t.Errorf("unauthorized dial classified as %q, want device_auth_required", got)
	}
	if !errors.Is(newDeviceDialError("h:1", ErrUserCancelled), ErrUserCancelled) {
		t.Error("a cancelled dial must still read as ErrUserCancelled")
	}
	if newDeviceDialError("h:1", nil) != nil {
		t.Error("newDeviceDialError(nil) must stay nil")
	}
}

func TestConnectResolvedAgentNamesTheDialedAddress(t *testing.T) {
	prev := dialAgentLadderFn
	dialAgentLadderFn = func(context.Context, dialTarget) (*grpcclient.AgentConnection, error, error) {
		return nil, nil, status.Error(codes.Unavailable, "connection refused")
	}
	t.Cleanup(func() { dialAgentLadderFn = prev })

	_, err := connectResolvedAgentWithProvisionedHint(context.Background(), "127.0.0.1", "127.0.0.1:1", false, func() bool { return false })
	var dial deviceAddresser
	if !errors.As(err, &dial) || dial.DeviceAddress() != "127.0.0.1:1" {
		t.Fatalf("err = %v, want it to name 127.0.0.1:1", err)
	}
	if got := ErrorClass(err); got != "device_unreachable" {
		t.Errorf("ErrorClass = %q, want device_unreachable", got)
	}
}

// A dial that timed out, or a host name that does not resolve, is still a
// verdict about the device, and must be classified as one before the generic
// deadline classes get to it.
func TestDeviceDialErrorClassifiesTimeoutsAndResolverMisses(t *testing.T) {
	for _, tc := range []struct {
		name  string
		addr  string
		cause error
		want  string
	}{
		{"gRPC deadline", "10.255.255.1:50051",
			status.Error(codes.DeadlineExceeded, "context deadline exceeded while waiting for connections to become ready"), "device_unreachable"},
		{"context deadline", "10.255.255.1:50051", fmt.Errorf("probing: %w", context.DeadlineExceeded), "device_unreachable"},
		{"resolver produced nothing", "nosuchhost.invalid:50051",
			status.Error(codes.Unavailable, "name resolver error: produced zero addresses"), "device_not_resolved"},
		{"DNS says no such host", "nosuchhost.invalid:50051",
			status.Error(codes.Unavailable, `connection error: desc = "transport: Error while dialing: dial tcp: lookup nosuchhost.invalid: no such host"`), "device_not_resolved"},
		// An mDNS name only resolves while its device is on the network.
		{"mDNS name that does not resolve", "wendyos-x.local:50051",
			status.Error(codes.Unavailable, "name resolver error: produced zero addresses"), "device_unreachable"},
		// A bare device name is resolved the same way (see isMDNSShapedHost),
		// so it too only resolves while the device is on the network.
		{"bare device name that does not resolve", "wendyos-zzz:50051",
			status.Error(codes.Unavailable, `connection error: desc = "transport: Error while dialing: dial tcp: lookup wendyos-zzz: no such host"`), "device_unreachable"},
		{"refused", "127.0.0.1:1", status.Error(codes.Unavailable, "connection refused"), "device_unreachable"},
		{"not a transport failure", "h:1", status.Error(codes.PermissionDenied, "no"), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := fmt.Errorf("connecting: %w", newDeviceDialError(tc.addr, tc.cause))
			if got := DeviceDialErrorClass(err); got != tc.want {
				t.Errorf("DeviceDialErrorClass = %q, want %q", got, tc.want)
			}
			if got := ErrorClass(err); got != tc.want {
				t.Errorf("ErrorClass = %q, want %q", got, tc.want)
			}
		})
	}
	if got := DeviceDialErrorClass(status.Error(codes.DeadlineExceeded, "x")); got != "" {
		t.Errorf("a deadline outside a device dial classified as %q, want unclassified", got)
	}
}
