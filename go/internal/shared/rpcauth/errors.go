// Package rpcauth identifies RPC caller-policy refusals across the agent/client boundary.
package rpcauth

import (
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	Domain         = "wendy.dev"
	IdentityDenied = "RPC_IDENTITY_DENIED"
)

// Denied retains PermissionDenied on the wire while distinguishing caller
// identity policy from application entitlement failures.
func Denied(message string) error {
	st := status.New(codes.PermissionDenied, message)
	detailed, err := st.WithDetails(&errdetails.ErrorInfo{Domain: Domain, Reason: IdentityDenied})
	if err != nil {
		return st.Err()
	}
	return detailed.Err()
}

func IsIdentityDenied(err error) bool {
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.PermissionDenied {
		return false
	}
	for _, detail := range st.Details() {
		if info, ok := detail.(*errdetails.ErrorInfo); ok && info.Domain == Domain && info.Reason == IdentityDenied {
			return true
		}
	}
	return false
}
