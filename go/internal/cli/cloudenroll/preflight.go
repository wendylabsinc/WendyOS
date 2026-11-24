package cloudenroll

import (
	"context"
	"fmt"
	"strings"

	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// CheckAgentEnrollment checks readiness without generating keys, reserving a
// Cloud asset or presenting EAB material. StartACMEProvisioning still enforces
// provisioning state and validates enrollment material after this read.
func CheckAgentEnrollment(ctx context.Context, conn grpc.ClientConnInterface) error {
	resp, err := agentpbv2.NewWendyProvisioningServiceClient(conn).IsProvisioned(ctx, &agentpbv2.IsProvisionedRequest{})
	if status.Code(err) == codes.Unimplemented {
		return fmt.Errorf("this agent does not support direct PKI enrollment; update the agent before enrolling")
	}
	if err != nil {
		return fmt.Errorf("checking device enrollment: %w", err)
	}
	if resp.GetProvisioned() != nil {
		return fmt.Errorf("device is already enrolled; no new enrollment was started")
	}
	state := resp.GetNotProvisioned()
	if state == nil {
		return fmt.Errorf("agent returned an unknown enrollment state")
	}
	if state.AcmeEnrollmentSupported == nil {
		return fmt.Errorf("this agent does not advertise direct PKI enrollment readiness; update the agent before enrolling (no Cloud reservation made)")
	}
	if !state.GetAcmeEnrollmentSupported() {
		// Agent-supplied text: %q keeps it to one escaped line in the terminal.
		if reason := state.GetAcmeEnrollmentUnavailableReason(); reason != "" {
			return fmt.Errorf("this device cannot enroll directly with PKI: %q (no Cloud reservation made)", reason)
		}
		return fmt.Errorf("this agent does not support direct PKI enrollment; update the agent before enrolling (no Cloud reservation made)")
	}
	return nil
}

// RequireClosedDeviceOwner refuses, before Cloud reserves the name, an enroll
// a Closed device would refuse: enrolling it needs the owner key. It runs on
// every connection: a caller without a key (the keyless listener) is refused
// too. An agent without access shapes is Open.
func RequireClosedDeviceOwner(ctx context.Context, conn grpc.ClientConnInterface) error {
	resp, err := agentpbv2.NewWendyAccessServiceClient(conn).GetAccess(ctx, &agentpbv2.GetAccessRequest{})
	if status.Code(err) == codes.Unimplemented {
		return nil
	}
	if err != nil {
		return fmt.Errorf("checking the device's access shape: %w", err)
	}
	if resp.GetShape() != agentpbv2.AccessShape_ACCESS_SHAPE_CLOSED {
		return nil
	}
	switch role := resp.GetCallerRole(); role {
	case agentpbv2.TrustedKeyRole_TRUSTED_KEY_ROLE_OWNER:
		return nil
	case agentpbv2.TrustedKeyRole_TRUSTED_KEY_ROLE_UNSPECIFIED:
		return fmt.Errorf("enrolling a closed device needs its owner key, and this connection has no key the device trusts; enroll from the owner's computer (no Cloud reservation made)")
	default:
		return fmt.Errorf("enrolling a closed device needs its owner key, and this computer's key has the %s role; enroll from the owner's computer (no Cloud reservation made)",
			strings.ToLower(strings.TrimPrefix(role.String(), "TRUSTED_KEY_ROLE_")))
	}
}
