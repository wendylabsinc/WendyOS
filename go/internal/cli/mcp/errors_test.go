package mcp

import (
	"fmt"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/rpcauth"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestErrResult_StructuredAndText(t *testing.T) {
	r := errResult(errCodeNotConnected, "no device connected")
	if !r.IsError {
		t.Fatal("expected IsError=true")
	}
	sc, ok := r.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("expected map structured content, got %T", r.StructuredContent)
	}
	if sc["error_code"] != "NOT_CONNECTED" {
		t.Errorf("error_code = %v, want NOT_CONNECTED", sc["error_code"])
	}
	text := toolResultText(t, r)
	if text != "[NOT_CONNECTED] no device connected" {
		t.Errorf("text = %q", text)
	}
}

func TestCodeFromGRPC(t *testing.T) {
	cases := map[codes.Code]errorCode{
		codes.Unavailable:      errCodeDeviceUnreachable,
		codes.PermissionDenied: errCodeEntitlementDenied,
		codes.NotFound:         errCodeNotFound,
		codes.InvalidArgument:  errCodeInvalidArgument,
		codes.Unimplemented:    errCodeUnsupported,
		codes.Unauthenticated:  errCodeAuthRequired,
		codes.Internal:         errCodeInternal,
	}
	for c, want := range cases {
		if got := codeFromGRPC(status.Error(c, "x")); got != want {
			t.Errorf("codeFromGRPC(%v) = %v, want %v", c, got, want)
		}
		// Cloud helpers wrap the RPC error with what they were doing.
		if got := codeFromGRPC(fmt.Errorf("listing devices: %w", status.Error(c, "x"))); got != want {
			t.Errorf("codeFromGRPC(wrapped %v) = %v, want %v", c, got, want)
		}
	}
}

func TestCodeFromGRPCDistinguishesRPCIdentityDenial(t *testing.T) {
	foreign, err := status.New(codes.PermissionDenied, "other policy").WithDetails(&errdetails.ErrorInfo{
		Domain: "other.example", Reason: rpcauth.IdentityDenied,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		err  error
		want errorCode
	}{
		{"device identity", rpcauth.Denied("sign in with operator credentials"), errCodeRPCIdentityDenied},
		{"app entitlement", status.Error(codes.PermissionDenied, "missing camera entitlement"), errCodeEntitlementDenied},
		{"foreign error domain", foreign.Err(), errCodeEntitlementDenied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Preserve ErrorInfo across the same protobuf boundary as an RPC.
			wire := status.FromProto(status.Convert(tc.err).Proto()).Err()
			for _, err := range []error{wire, fmt.Errorf("calling agent: %w", wire)} {
				if got := codeFromGRPC(err); got != tc.want {
					t.Fatalf("codeFromGRPC(%v)=%s, want %s", err, got, tc.want)
				}
			}
		})
	}
}
