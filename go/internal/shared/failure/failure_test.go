package failure

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"regexp"
	"syscall"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestDescribeWrappedErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want Info
	}{
		{"login", status.Error(codes.Unauthenticated, "arbitrary wording"), Info{AuthenticationRequired, SignIn}},
		{"access", status.Error(codes.PermissionDenied, "offline"), Info{AccessDenied, ChooseAccount}},
		{"update", status.Error(codes.Unimplemented, "missing RPC"), Info{AgentUpdateRequired, UpdateAgent}},
		{"network", &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}, Info{NetworkUnavailable, Retry}},
		{"space", syscall.ENOSPC, Info{StorageFull, FreeStorage}},
		{"port", syscall.EADDRINUSE, Info{PortInUse, ChoosePort}},
		{"cancel", context.Canceled, Info{Cancelled, None}},
		{"busy", status.Error(codes.ResourceExhausted, "quota"), Info{ServiceBusy, Wait}},
		{"text is not evidence", errors.New("sign in: certificate expired: permission denied: no route to host"), Info{Unknown, None}},
		{"explicit overrides cause", Wrap(syscall.ECONNRESET, IdentityMismatch, ReviewIdentity), Info{IdentityMismatch, ReviewIdentity}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Describe(fmt.Errorf("outer context: %w", tc.err)); got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
	cause := errors.New("original")
	wrapped := Wrap(cause, AuthenticationRequired, SignIn)
	if !errors.Is(wrapped, cause) || wrapped.Error() != cause.Error() {
		t.Fatal("wrapper lost cause or diagnostic")
	}
	if Wrap(nil, Unknown, None) != nil {
		t.Fatal("nil became a failure")
	}
}

// The native bridge casts these values directly into C. Detect accidental ABI
// drift independently of the Swift tests, which compare Swift with this header.
func TestNativeABIValues(t *testing.T) {
	header, err := os.ReadFile("../../../macos/Sources/Shared/BridgeTypes.h")
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]int32{
		"WD_ERROR_UNKNOWN": int32(Unknown), "WD_ERROR_AUTHENTICATION_REQUIRED": int32(AuthenticationRequired),
		"WD_ERROR_CREDENTIALS_EXPIRED": int32(CredentialsExpired), "WD_ERROR_ACCOUNT_MISMATCH": int32(AccountMismatch),
		"WD_ERROR_ACCESS_DENIED": int32(AccessDenied), "WD_ERROR_IDENTITY_MISMATCH": int32(IdentityMismatch),
		"WD_ERROR_SECURE_CONNECTION_REJECTED": int32(SecureConnectionRejected), "WD_ERROR_NETWORK_UNAVAILABLE": int32(NetworkUnavailable),
		"WD_ERROR_LOCAL_NETWORK_DENIED": int32(LocalNetworkDenied), "WD_ERROR_AGENT_UPDATE_REQUIRED": int32(AgentUpdateRequired),
		"WD_ERROR_STORAGE_FULL": int32(StorageFull), "WD_ERROR_PORT_IN_USE": int32(PortInUse), "WD_ERROR_SERVICE_BUSY": int32(ServiceBusy), "WD_ERROR_CANCELLED": int32(Cancelled),
		"WD_REMEDY_NONE": int32(None), "WD_REMEDY_RETRY": int32(Retry), "WD_REMEDY_SIGN_IN": int32(SignIn),
		"WD_REMEDY_CHOOSE_ACCOUNT": int32(ChooseAccount), "WD_REMEDY_OPEN_NETWORK_SETTINGS": int32(OpenNetworkSettings),
		"WD_REMEDY_CHECK_CLOCK": int32(CheckClock), "WD_REMEDY_UPDATE_AGENT": int32(UpdateAgent), "WD_REMEDY_REVIEW_IDENTITY": int32(ReviewIdentity),
		"WD_REMEDY_FREE_STORAGE": int32(FreeStorage), "WD_REMEDY_CHOOSE_PORT": int32(ChoosePort), "WD_REMEDY_WAIT": int32(Wait),
	}
	for name, value := range values {
		pattern := fmt.Sprintf(`\b%s\s*=\s*%d\b`, name, value)
		if !regexp.MustCompile(pattern).Match(header) {
			t.Errorf("C ABI differs for %s = %d", name, value)
		}
	}
}
