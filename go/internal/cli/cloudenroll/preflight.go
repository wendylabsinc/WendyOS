package cloudenroll

import (
	"context"
	"fmt"

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
		return fmt.Errorf("this agent does not support direct PKI enrollment; update the agent before enrolling (no Cloud reservation made)")
	}
	return nil
}
