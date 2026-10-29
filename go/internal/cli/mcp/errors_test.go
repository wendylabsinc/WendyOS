package mcp

import (
	"fmt"
	"testing"

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
