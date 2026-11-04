package sessionbroker

import (
	"errors"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const operatorErrorDomain = "wendy.sessionbroker"

// These bounded reasons describe local route readiness, not peer trust failures.
const (
	operatorNoPeers       = "OPERATOR_NO_PEERS"
	operatorReconnecting  = "OPERATOR_RECONNECTING"
	operatorTargetPending = "OPERATOR_TARGET_PENDING"
	operatorViewPartial   = "OPERATOR_OBSERVATION_INCOMPLETE"
)

func operatorUnavailable(reason, message string) error {
	st := status.New(codes.Unavailable, message)
	with, err := st.WithDetails(&errdetails.ErrorInfo{Domain: operatorErrorDomain, Reason: reason})
	if err != nil {
		return st.Err()
	}
	return with.Err()
}

// OperatorRecovery recognizes only explicit local-broker readiness errors.
// Generic Unavailable and authentication/policy failures retain their own class.
// Details survive the private gRPC proxy boundary without matching error text.
func OperatorRecovery(err error) (bool, []string) {
	if operatorPolicyFailure(err) {
		return false, nil
	}
	var startup *operatorStartupFailure
	if errors.As(err, &startup) {
		switch startup.failure.Kind {
		case "timeout", "connection_closed":
			return true, []string{"Check that nearby Wendy Net routers are reachable over LAN or Bluetooth and participating, then retry the connection."}
		case "issuer_chain", "certificate":
			return false, []string{"Check the selected organization, the router certificate chain and the device clock; correct the reported verification error before reconnecting."}
		case "device_pin":
			return false, []string{"Verify that this is the expected device and investigate why its certificate differs from the saved certificate pin before reconnecting."}
		default:
			return false, []string{"Check that nearby routers have LAN or Bluetooth enabled and participate in the same Wendy Net organization, then retry automatic discovery."}
		}
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.Unavailable {
		return false, nil
	}
	for _, detail := range st.Details() {
		info, ok := detail.(*errdetails.ErrorInfo)
		if !ok || info.Domain != operatorErrorDomain {
			continue
		}
		switch info.Reason {
		case operatorNoPeers, operatorReconnecting:
			return true, []string{
				"Check that nearby routers are reachable over LAN or Bluetooth and participate in the same Wendy Net organization.",
				"Retry while the local router discovers and reconnects automatically.",
			}
		case operatorTargetPending:
			return true, []string{
				"Wait for a verified route and management address for the device, then retry.",
				"Check that the target is running and participating in the same Wendy Net organization.",
			}
		case operatorViewPartial:
			return false, []string{"Retry discovery or use a direct LAN connection; the incomplete device list cannot establish whether this device is reachable."}
		}
	}
	return false, nil
}

func operatorPolicyFailure(err error) bool {
	if err == nil {
		return false
	}
	if st, ok := status.FromError(err); ok && (st.Code() == codes.Unauthenticated || st.Code() == codes.PermissionDenied) {
		return true
	}
	switch wrapped := err.(type) {
	case interface{ Unwrap() []error }:
		for _, inner := range wrapped.Unwrap() {
			if operatorPolicyFailure(inner) {
				return true
			}
		}
	case interface{ Unwrap() error }:
		return operatorPolicyFailure(wrapped.Unwrap())
	}
	return false
}
